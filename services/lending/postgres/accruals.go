package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// InsertAccrual records interest recognised for one loan on one business
// date. The primary key makes a second run for the same date a no-op; it
// returns false in that case.
func (q *Queries) InsertAccrual(ctx context.Context, loanID uuid.UUID, date time.Time, amount int64) (bool, error) {
	tag, err := q.db.Exec(ctx, `
		INSERT INTO lending.interest_accruals (loan_id, business_date, amount_minor) VALUES ($1,$2,$3)
		ON CONFLICT (loan_id, business_date) DO NOTHING`, loanID, date, amount)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AccrualTotal sums the per-loan accruals of a business date.
func (q *Queries) AccrualTotal(ctx context.Context, date time.Time) (total int64, loans int, err error) {
	err = q.db.QueryRow(ctx, `
		SELECT coalesce(sum(amount_minor),0)::bigint, count(*) FROM lending.interest_accruals WHERE business_date = $1`, date).Scan(&total, &loans)
	return total, loans, err
}

// AccrualRun is the aggregate accrual of one business date.
type AccrualRun struct {
	BusinessDate time.Time
	State        string // CALCULATED | POSTED | EMPTY
	TotalMinor   int64
	Loans        int
	IntentID     *uuid.UUID
}

func (q *Queries) GetAccrualRun(ctx context.Context, date time.Time) (AccrualRun, bool, error) {
	var r AccrualRun
	err := q.db.QueryRow(ctx, `
		SELECT business_date, state, total_minor, loans, intent_id FROM lending.accrual_runs WHERE business_date = $1`, date).
		Scan(&r.BusinessDate, &r.State, &r.TotalMinor, &r.Loans, &r.IntentID)
	if err == pgx.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

func (q *Queries) InsertAccrualRun(ctx context.Context, r AccrualRun) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO lending.accrual_runs (business_date, state, total_minor, loans, intent_id) VALUES ($1,$2,$3,$4,$5)`,
		r.BusinessDate, r.State, r.TotalMinor, r.Loans, r.IntentID)
	return err
}

func (q *Queries) MarkAccrualRunPosted(ctx context.Context, intentID uuid.UUID, now time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE lending.accrual_runs SET state = 'POSTED', posted_at = $2 WHERE intent_id = $1`, intentID, now)
	return err
}

// LatestAccrualDate returns the most recent business date with an accrual run.
func (q *Queries) LatestAccrualDate(ctx context.Context) (time.Time, bool, error) {
	var d *time.Time
	if err := q.db.QueryRow(ctx, `SELECT max(business_date) FROM lending.accrual_runs`).Scan(&d); err != nil {
		return time.Time{}, false, err
	}
	if d == nil {
		return time.Time{}, false, nil
	}
	return *d, true, nil
}

// Accruals lists a loan's recognised interest by date, for statements.
func (q *Queries) Accruals(ctx context.Context, loanID uuid.UUID) (map[time.Time]int64, error) {
	rows, err := q.db.Query(ctx, `SELECT business_date, amount_minor FROM lending.interest_accruals WHERE loan_id = $1`, loanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[time.Time]int64{}
	for rows.Next() {
		var (
			d time.Time
			a int64
		)
		if err := rows.Scan(&d, &a); err != nil {
			return nil, err
		}
		out[d] = a
	}
	return out, rows.Err()
}

// UnpostedAccrualDates counts business dates that have per-loan accruals but
// no posted aggregate journal yet. Control-account reconciliation waits for
// this to be zero.
func (q *Queries) UnpostedAccrualDates(ctx context.Context) (int, error) {
	var n int
	err := q.db.QueryRow(ctx, `
		SELECT count(DISTINCT a.business_date)
		  FROM lending.interest_accruals a
		  LEFT JOIN lending.accrual_runs r USING (business_date)
		 WHERE r.state IS DISTINCT FROM 'POSTED'`).Scan(&n)
	return n, err
}
