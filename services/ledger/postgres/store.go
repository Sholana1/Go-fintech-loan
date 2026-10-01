// Package postgres implements the ledger's posting transactions.
//
// Responsibility: every change to money happens in one of the transaction
// scripts in this file. Each script follows the same protocol, which is what
// makes the ledger's invariants hold under concurrency:
//
//  1. Claim the operation reference (unique insert). A repeat finds the
//     claim and returns the original result: nothing is posted twice.
//  2. Lock the balance rows of every account involved, in one deterministic
//     order (customer accounts by id, then system accounts by id). Two
//     transactions over the same accounts therefore queue instead of
//     deadlocking, and no one else can change those balances until commit.
//  3. Check account status and available funds against the locked rows.
//     Because the rows are locked, the check and the write are one atomic
//     step: a cached or replicated balance is never consulted.
//  4. Insert the journal and its entries. A database trigger applies each
//     entry to the balance row; the application cannot write balances.
//  5. Insert the outbox event in the same transaction.
//  6. Commit. A deferred constraint trigger verifies the journal balances
//     per currency, and a CHECK on balances is the final backstop on funds.
//
// Consistency boundary: one PostgreSQL transaction at READ COMMITTED with
// explicit row locks. No network call is made while locks are held.
//
// Failure modes: lock or statement timeout, serialization failure and
// deadlock surface as errors; TxRunner retries the last two, which is safe
// because of step 1. A crash at any point leaves either the whole operation
// or none of it.
package postgres

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"bankplatform.internal/platform/pgxutil"
)

const outboxTable = "ledger.outbox"

// Store executes ledger transactions.
type Store struct {
	pool *pgxpool.Pool
	tx   pgxutil.TxRunner
	now  func() time.Time
}

// NewStore returns a Store. onRetry is called whenever a transaction is
// retried after a serialization failure or deadlock.
func NewStore(pool *pgxpool.Pool, now func() time.Time, onRetry func(code string, attempt int)) *Store {
	return &Store{pool: pool, now: now, tx: pgxutil.TxRunner{Pool: pool, MaxAttempts: 3, OnRetry: onRetry}}
}
