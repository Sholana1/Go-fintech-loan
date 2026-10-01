package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// MarkEventProcessed inserts the consumer's inbox row. It returns false when
// the event was already processed, in which case the caller must skip its
// effect. Called inside the same transaction as the effect, this is what
// makes a redelivered event harmless.
func (q *Queries) MarkEventProcessed(ctx context.Context, consumer string, eventID uuid.UUID) (bool, error) {
	tag, err := q.db.Exec(ctx, `
		INSERT INTO lending.inbox (consumer, event_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, consumer, eventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// JobCompleted reports whether a daily job has completed for a date.
func (q *Queries) JobCompleted(ctx context.Context, job string, date time.Time) (bool, error) {
	var done bool
	err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lending.job_runs WHERE job = $1 AND business_date = $2)`, job, date).Scan(&done)
	return done, err
}

// LatestJobDate returns the most recent business date a job completed for.
func (q *Queries) LatestJobDate(ctx context.Context, job string) (time.Time, bool, error) {
	var d *time.Time
	if err := q.db.QueryRow(ctx, `SELECT max(business_date) FROM lending.job_runs WHERE job = $1`, job).Scan(&d); err != nil {
		return time.Time{}, false, err
	}
	if d == nil {
		return time.Time{}, false, nil
	}
	return *d, true, nil
}

// RecordJobRun marks a daily job complete for a date. Idempotent.
func (q *Queries) RecordJobRun(ctx context.Context, job string, date time.Time, summary map[string]any) error {
	raw, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `
		INSERT INTO lending.job_runs (job, business_date, summary) VALUES ($1,$2,$3)
		ON CONFLICT (job, business_date) DO NOTHING`, job, date, raw)
	return err
}

// AdvisoryLock takes a transaction-scoped advisory lock, waiting for it. With
// shared=true many holders may coexist; an exclusive holder waits for all of
// them and blocks new ones. The accrual job uses this to make "no more rows
// will be written for this date" true before it totals the date.
// Must be called inside InTx; the lock is released at commit or rollback.
func (q *Queries) AdvisoryLock(ctx context.Context, key int64, shared bool) error {
	if q.tx == nil {
		return errors.New("lending: advisory lock requires a transaction")
	}
	stmt := `SELECT pg_advisory_xact_lock($1)`
	if shared {
		stmt = `SELECT pg_advisory_xact_lock_shared($1)`
	}
	_, err := q.db.Exec(ctx, stmt, key)
	return err
}
