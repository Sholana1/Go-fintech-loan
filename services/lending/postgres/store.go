// Package postgres is lending's persistence.
//
// Shape: Store owns the pool and the transaction runner. Queries holds every
// SQL statement and works against either the pool (single-statement reads
// and writes) or a transaction (inside Store.InTx). Use cases in the app
// package compose Queries methods inside InTx, so each use case's
// transaction boundary is visible where the business steps are.
//
// There is deliberately no generic repository: each method is one statement
// written for one purpose, with its locking and its compare-and-set
// conditions spelled out.
//
// Conventions:
//   - State changes are compare-and-set (`WHERE state = $expected`). A method
//     that performs one returns whether it changed a row; false means another
//     worker moved the row first and the caller must re-read.
//   - Work queues are claimed with FOR UPDATE SKIP LOCKED plus a lease
//     (next_attempt_at pushed into the future), so two workers never drive
//     the same row and a crashed worker's row becomes claimable again.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"bankplatform.internal/platform/outbox"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/lending/domain"
)

// OutboxTable is lending's outbox.
const OutboxTable = "lending.outbox"

// db is the subset of pgx shared by a pool and a transaction.
type db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Queries is the set of SQL operations, bound to a pool or a transaction.
type Queries struct {
	db db
	tx pgx.Tx // nil when bound to the pool
}

// Store is lending's database handle.
type Store struct {
	*Queries
	pool   *pgxpool.Pool
	runner pgxutil.TxRunner
}

// NewStore returns a Store. onRetry observes transaction retries.
func NewStore(pool *pgxpool.Pool, onRetry func(code string, attempt int)) *Store {
	return &Store{
		Queries: &Queries{db: pool},
		pool:    pool,
		runner:  pgxutil.TxRunner{Pool: pool, MaxAttempts: 3, OnRetry: onRetry},
	}
}

// InTx runs fn in one READ COMMITTED transaction. fn must not make network
// calls: row locks are held until it returns.
func (s *Store) InTx(ctx context.Context, fn func(q *Queries) error) error {
	return s.runner.InTx(ctx, func(tx pgx.Tx) error {
		return fn(&Queries{db: tx, tx: tx})
	})
}

// Pool exposes the pool to components that need their own connection (the
// outbox relay) and to readiness checks.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Emit writes a domain event to the outbox. It must be called inside InTx so
// the event commits with the change it describes.
func (q *Queries) Emit(ctx context.Context, e outbox.Event) error {
	if q.tx == nil {
		return errors.New("lending: Emit must be called inside a transaction")
	}
	_, err := outbox.Insert(ctx, q.tx, OutboxTable, e)
	return err
}

func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", domain.ErrNotFound, what)
	}
	return err
}
