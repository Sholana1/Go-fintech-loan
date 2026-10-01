// Package postgres is identity's persistence. SQL is written out per use
// case so that locking and constraints are visible where they matter:
// credential checks run under a row lock so that concurrent guesses cannot
// exceed the lockout threshold.
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"bankplatform.internal/platform/pgxutil"
)

type Store struct {
	pool *pgxpool.Pool
	tx   pgxutil.TxRunner
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, tx: pgxutil.TxRunner{Pool: pool}}
}

// Ping is the readiness check.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
