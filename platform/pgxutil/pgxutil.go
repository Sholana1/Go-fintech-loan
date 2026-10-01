// Package pgxutil holds the small amount of PostgreSQL plumbing every service
// repeats: building a bounded pool with server-side timeouts, and running a
// transaction with explicit, observable retries on serialization failures
// and deadlocks.
//
// It deliberately contains no query helpers or generic repositories: each
// service writes its own SQL so that locking and constraint behaviour stays
// visible at the call site.
package pgxutil

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL error codes this platform reacts to.
const (
	CodeSerializationFailure = "40001"
	CodeDeadlockDetected     = "40P01"
	CodeUniqueViolation      = "23505"
	CodeCheckViolation       = "23514"
	CodeLockNotAvailable     = "55P03"
	CodeQueryCanceled        = "57014"
)

// PoolOptions bounds a connection pool and sets server-side timeouts so a
// stuck statement or lock wait fails fast instead of holding a connection.
type PoolOptions struct {
	ApplicationName  string
	MaxConns         int32
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	// IdleInTxTimeout kills sessions that sit idle inside a transaction,
	// which would otherwise hold row locks indefinitely after an app bug.
	IdleInTxTimeout time.Duration
}

// NewPool builds a pool and verifies connectivity.
func NewPool(ctx context.Context, dsn string, o PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	rp := cfg.ConnConfig.RuntimeParams
	if o.ApplicationName != "" {
		rp["application_name"] = o.ApplicationName
	}
	if o.StatementTimeout > 0 {
		rp["statement_timeout"] = fmt.Sprintf("%d", o.StatementTimeout.Milliseconds())
	}
	if o.LockTimeout > 0 {
		rp["lock_timeout"] = fmt.Sprintf("%d", o.LockTimeout.Milliseconds())
	}
	if o.IdleInTxTimeout > 0 {
		rp["idle_in_transaction_session_timeout"] = fmt.Sprintf("%d", o.IdleInTxTimeout.Milliseconds())
	}
	rp["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// TxRunner runs functions inside a READ COMMITTED transaction and retries the
// whole function when PostgreSQL reports a serialization failure or deadlock.
//
// Retrying is only correct because every transaction in this platform is
// idempotent by construction (it starts by claiming a unique reference). The
// retry is explicit and observable: OnRetry is called for each one.
type TxRunner struct {
	Pool        *pgxpool.Pool
	MaxAttempts int                            // total attempts; defaults to 3
	OnRetry     func(code string, attempt int) // optional; for metrics and logs
}

// InTx runs fn in a transaction. fn must not perform network calls other
// than through tx: locks are held until it returns.
func (r TxRunner) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	attempts := r.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = r.once(ctx, fn)
		code := Code(err)
		if code != CodeSerializationFailure && code != CodeDeadlockDetected {
			return err
		}
		if attempt == attempts {
			break
		}
		if r.OnRetry != nil {
			r.OnRetry(code, attempt)
		}
		// Jittered backoff so two colliding transactions do not collide again.
		wait := time.Duration(5+rand.IntN(20*attempt)) * time.Millisecond
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(wait):
		}
	}
	return err
}

func (r TxRunner) once(ctx context.Context, fn func(tx pgx.Tx) error) (err error) {
	tx, err := r.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// Roll back with a fresh context: the request context may
			// already be cancelled, and the rollback must still be sent.
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_ = tx.Rollback(rbCtx)
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Code returns the SQLSTATE of err, or "" if err is not a PostgreSQL error.
func Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Constraint returns the violated constraint name, or "".
func Constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// IsUniqueViolation reports whether err violates the named unique constraint
// or index. An empty name matches any unique violation.
func IsUniqueViolation(err error, constraint string) bool {
	if Code(err) != CodeUniqueViolation {
		return false
	}
	return constraint == "" || Constraint(err) == constraint
}
