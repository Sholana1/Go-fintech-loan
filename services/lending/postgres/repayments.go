package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/services/lending/domain"
)

const repaymentColumns = `repayment_id, loan_id, customer_id, source, requested_minor, applied_minor, unapplied_minor,
	allocation, is_recovery, state, coalesce(reject_reason,''), journal_id, business_date, intent_id, created_at, completed_at`

func scanRepayment(row pgx.Row) (domain.Repayment, error) {
	var (
		r     domain.Repayment
		state string
		alloc []byte
	)
	err := row.Scan(&r.ID, &r.LoanID, &r.CustomerID, &r.Source, &r.RequestedMinor, &r.AppliedMinor, &r.UnappliedMinor,
		&alloc, &r.IsRecovery, &state, &r.RejectReason, &r.JournalID, &r.BusinessDate, &r.IntentID, &r.CreatedAt, &r.CompletedAt)
	if err != nil {
		return r, err
	}
	r.State = domain.RepaymentState(state)
	return r, json.Unmarshal(alloc, &r.Allocation)
}

func (q *Queries) InsertRepayment(ctx context.Context, r domain.Repayment) error {
	alloc, err := json.Marshal(r.Allocation)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `
		INSERT INTO lending.repayments (repayment_id, loan_id, customer_id, source, requested_minor, applied_minor, unapplied_minor,
			allocation, is_recovery, state, business_date, intent_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		r.ID, r.LoanID, r.CustomerID, r.Source, r.RequestedMinor, r.AppliedMinor, r.UnappliedMinor,
		alloc, r.IsRecovery, string(r.State), r.BusinessDate, r.IntentID, r.CreatedAt)
	return err
}

func (q *Queries) GetRepayment(ctx context.Context, id uuid.UUID) (domain.Repayment, error) {
	r, err := scanRepayment(q.db.QueryRow(ctx, `SELECT `+repaymentColumns+` FROM lending.repayments WHERE repayment_id = $1`, id))
	return r, notFound(err, "repayment")
}

func (q *Queries) RepaymentByIntent(ctx context.Context, intentID uuid.UUID) (domain.Repayment, error) {
	r, err := scanRepayment(q.db.QueryRow(ctx, `SELECT `+repaymentColumns+` FROM lending.repayments WHERE intent_id = $1`, intentID))
	return r, notFound(err, "repayment")
}

// CompleteRepayment records the outcome of a repayment's posting.
func (q *Queries) CompleteRepayment(ctx context.Context, id uuid.UUID, state domain.RepaymentState, journalID *int64, rejectReason string, now time.Time) error {
	_, err := q.db.Exec(ctx, `
		UPDATE lending.repayments SET state = $2, journal_id = $3, reject_reason = nullif($4,''), completed_at = $5
		 WHERE repayment_id = $1 AND state = 'PENDING'`, id, string(state), journalID, rejectReason, now)
	return err
}

// Repayments lists a loan's repayments, oldest first.
func (q *Queries) Repayments(ctx context.Context, loanID uuid.UUID) ([]domain.Repayment, error) {
	rows, err := q.db.Query(ctx, `SELECT `+repaymentColumns+` FROM lending.repayments WHERE loan_id = $1 ORDER BY created_at`, loanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Repayment
	for rows.Next() {
		r, err := scanRepayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
