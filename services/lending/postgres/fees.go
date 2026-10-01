package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// InsertLateFee records a late fee to be posted. It returns false when a late
// fee already exists for that instalment (the unique constraint), which is
// how the daily job stays idempotent.
func (q *Queries) InsertLateFee(ctx context.Context, feeID, loanID uuid.UUID, dueDate time.Time, amount int64, intentID uuid.UUID) (bool, error) {
	tag, err := q.db.Exec(ctx, `
		INSERT INTO lending.loan_fees (fee_id, loan_id, kind, instalment_due_date, amount_minor, state, intent_id)
		VALUES ($1,$2,'LATE_FEE',$3,$4,'PENDING',$5)
		ON CONFLICT (loan_id, kind, instalment_due_date) DO NOTHING`, feeID, loanID, dueDate, amount, intentID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LateFeeExists reports whether a late fee was already assessed.
func (q *Queries) LateFeeExists(ctx context.Context, loanID uuid.UUID, dueDate time.Time) (bool, error) {
	var exists bool
	err := q.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM lending.loan_fees WHERE loan_id = $1 AND kind = 'LATE_FEE' AND instalment_due_date = $2)`, loanID, dueDate).Scan(&exists)
	return exists, err
}

func (q *Queries) CompleteLateFee(ctx context.Context, intentID uuid.UUID, state string) error {
	_, err := q.db.Exec(ctx, `UPDATE lending.loan_fees SET state = $2 WHERE intent_id = $1 AND state = 'PENDING'`, intentID, state)
	return err
}

// LoanFee is a fee charged on a loan, for statements.
type LoanFee struct {
	Kind        string
	DueDate     time.Time
	AmountMinor int64
	State       string
	CreatedAt   time.Time
}

func (q *Queries) LoanFees(ctx context.Context, loanID uuid.UUID) ([]LoanFee, error) {
	rows, err := q.db.Query(ctx, `
		SELECT kind, instalment_due_date, amount_minor, state, created_at FROM lending.loan_fees WHERE loan_id = $1 ORDER BY created_at`, loanID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LoanFee, error) {
		var f LoanFee
		err := row.Scan(&f.Kind, &f.DueDate, &f.AmountMinor, &f.State, &f.CreatedAt)
		return f, err
	})
}
