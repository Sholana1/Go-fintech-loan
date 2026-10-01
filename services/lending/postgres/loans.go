package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/services/lending/domain"
)

const loanColumns = `loan_id, application_id, offer_id, customer_id, product_id, product_version, principal_minor,
	monthly_rate_bps, tenor_months, origination_fee_minor, state, schedule_version, auto_debit_authorised,
	deposit_account_code, accepted_on, disbursed_at, days_past_due, arrears_bucket, restructured,
	written_off_minor, recovered_minor, closed_at, accrual_catchup, version`

func scanLoan(row pgx.Row) (domain.Loan, error) {
	var (
		l     domain.Loan
		state string
	)
	err := row.Scan(&l.ID, &l.ApplicationID, &l.OfferID, &l.CustomerID, &l.ProductID, &l.ProductVersion, &l.PrincipalMinor,
		&l.MonthlyRateBps, &l.TenorMonths, &l.OriginationFeeMinor, &state, &l.ScheduleVersion, &l.AutoDebitAuthorised,
		&l.DepositAccountCode, &l.AcceptedOn, &l.DisbursedAt, &l.DaysPastDue, &l.ArrearsBucket, &l.Restructured,
		&l.WrittenOffMinor, &l.RecoveredMinor, &l.ClosedAt, &l.AccrualCatchup, &l.Version)
	l.State = domain.LoanState(state)
	return l, err
}

// InsertLoan stores a new loan in PENDING_DISBURSEMENT. The unique
// constraints on application_id and offer_id guarantee one loan per
// application and per offer.
func (q *Queries) InsertLoan(ctx context.Context, l domain.Loan) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO lending.loans (loan_id, application_id, offer_id, customer_id, product_id, product_version, principal_minor,
			monthly_rate_bps, tenor_months, origination_fee_minor, state, schedule_version, auto_debit_authorised,
			deposit_account_code, accepted_on)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		l.ID, l.ApplicationID, l.OfferID, l.CustomerID, l.ProductID, l.ProductVersion, l.PrincipalMinor,
		l.MonthlyRateBps, l.TenorMonths, l.OriginationFeeMinor, string(l.State), l.ScheduleVersion, l.AutoDebitAuthorised,
		l.DepositAccountCode, l.AcceptedOn)
	return err
}

func (q *Queries) GetLoan(ctx context.Context, id uuid.UUID) (domain.Loan, error) {
	l, err := scanLoan(q.db.QueryRow(ctx, `SELECT `+loanColumns+` FROM lending.loans WHERE loan_id = $1`, id))
	return l, notFound(err, "loan")
}

// GetLoanForCustomer returns the loan only if it belongs to the customer.
func (q *Queries) GetLoanForCustomer(ctx context.Context, id, customerID uuid.UUID) (domain.Loan, error) {
	l, err := scanLoan(q.db.QueryRow(ctx, `SELECT `+loanColumns+` FROM lending.loans WHERE loan_id = $1 AND customer_id = $2`, id, customerID))
	return l, notFound(err, "loan")
}

func (q *Queries) LoanByApplication(ctx context.Context, applicationID uuid.UUID) (domain.Loan, error) {
	l, err := scanLoan(q.db.QueryRow(ctx, `SELECT `+loanColumns+` FROM lending.loans WHERE application_id = $1`, applicationID))
	return l, notFound(err, "loan")
}

// LockLoan reads the loan FOR UPDATE. Every change to a loan's schedule or
// state happens under this lock, which serialises repayments, accrual,
// arrears classification and administrative actions on the same loan.
func (q *Queries) LockLoan(ctx context.Context, id uuid.UUID) (domain.Loan, error) {
	l, err := scanLoan(q.db.QueryRow(ctx, `SELECT `+loanColumns+` FROM lending.loans WHERE loan_id = $1 FOR UPDATE`, id))
	return l, notFound(err, "loan")
}

// Instalments returns one schedule version in sequence order.
func (q *Queries) Instalments(ctx context.Context, loanID uuid.UUID, version int) ([]domain.Instalment, error) {
	rows, err := q.db.Query(ctx, `
		SELECT seq, period_start, due_date, principal_due, interest_due, fees_due, principal_paid, interest_paid, fees_paid,
		       interest_accrued, accrual_from, accrual_base
		  FROM lending.instalments WHERE loan_id = $1 AND schedule_version = $2 ORDER BY seq`, loanID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Instalment
	for rows.Next() {
		var i domain.Instalment
		if err := rows.Scan(&i.Seq, &i.PeriodStart, &i.DueDate, &i.PrincipalDue, &i.InterestDue, &i.FeesDue,
			&i.PrincipalPaid, &i.InterestPaid, &i.FeesPaid, &i.InterestAccrued, &i.AccrualFrom, &i.AccrualBase); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// InsertInstalments writes a complete schedule version.
func (q *Queries) InsertInstalments(ctx context.Context, loanID uuid.UUID, version int, insts []domain.Instalment) error {
	for _, i := range insts {
		if _, err := q.db.Exec(ctx, `
			INSERT INTO lending.instalments (loan_id, schedule_version, seq, period_start, due_date, principal_due, interest_due, fees_due,
				principal_paid, interest_paid, fees_paid, interest_accrued, accrual_from, accrual_base)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			loanID, version, i.Seq, i.PeriodStart, i.DueDate, i.PrincipalDue, i.InterestDue, i.FeesDue,
			i.PrincipalPaid, i.InterestPaid, i.FeesPaid, i.InterestAccrued, i.AccrualFrom, i.AccrualBase); err != nil {
			return err
		}
	}
	return nil
}

// SaveInstalments updates the paid, fee and accrual columns of existing
// instalments. The CHECK constraints (paid <= due, accrued <= due) are the
// database's backstop against an over-allocation.
func (q *Queries) SaveInstalments(ctx context.Context, loanID uuid.UUID, version int, insts []domain.Instalment) error {
	for _, i := range insts {
		tag, err := q.db.Exec(ctx, `
			UPDATE lending.instalments
			   SET principal_paid = $4, interest_paid = $5, fees_paid = $6, fees_due = $7, interest_accrued = $8
			 WHERE loan_id = $1 AND schedule_version = $2 AND seq = $3`,
			loanID, version, i.Seq, i.PrincipalPaid, i.InterestPaid, i.FeesPaid, i.FeesDue, i.InterestAccrued)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return notFound(pgx.ErrNoRows, "instalment")
		}
	}
	return nil
}

// LoanUpdate is the set of loan columns a use case may change. Only non-nil
// fields are written.
type LoanUpdate struct {
	State            *domain.LoanState
	ScheduleVersion  *int
	DisbursedAt      *time.Time
	JournalID        *int64
	DaysPastDue      *int
	ArrearsBucket    *string
	Restructured     *bool
	WrittenOffMinor  *int64
	RecoveredMinor   *int64
	ClosedAt         *time.Time
	AccrualCatchup   *bool
	NextCollectionAt **time.Time
}

// UpdateLoan writes the given fields. The caller holds the loan's row lock
// (LockLoan), which is what makes read-modify-write on a loan safe.
func (q *Queries) UpdateLoan(ctx context.Context, id uuid.UUID, u LoanUpdate) error {
	var state *string
	if u.State != nil {
		s := string(*u.State)
		state = &s
	}
	setCollection := u.NextCollectionAt != nil
	var nextCollection *time.Time
	if setCollection {
		nextCollection = *u.NextCollectionAt
	}
	_, err := q.db.Exec(ctx, `
		UPDATE lending.loans SET
		  state            = coalesce($2, state),
		  schedule_version = coalesce($3, schedule_version),
		  disbursed_at     = coalesce($4, disbursed_at),
		  disbursement_journal_id = coalesce($5, disbursement_journal_id),
		  days_past_due    = coalesce($6, days_past_due),
		  arrears_bucket   = coalesce($7, arrears_bucket),
		  restructured     = coalesce($8, restructured),
		  written_off_minor = coalesce($9, written_off_minor),
		  recovered_minor  = coalesce($10, recovered_minor),
		  closed_at        = coalesce($11, closed_at),
		  next_collection_at = CASE WHEN $12 THEN $13 ELSE next_collection_at END,
		  accrual_catchup  = coalesce($14, accrual_catchup),
		  version = version + 1
		WHERE loan_id = $1`,
		id, state, u.ScheduleVersion, u.DisbursedAt, u.JournalID, u.DaysPastDue, u.ArrearsBucket, u.Restructured,
		u.WrittenOffMinor, u.RecoveredMinor, u.ClosedAt, setCollection, nextCollection, u.AccrualCatchup)
	return err
}

// Exposure summarises a customer's existing lending relationship.
type Exposure struct {
	OutstandingPrincipalMinor int64
	HasActiveLoan             bool
	HasPriorWriteOff          bool
}

// CustomerExposure returns what the credit policy needs to know about the
// customer's other loans with us.
func (q *Queries) CustomerExposure(ctx context.Context, customerID uuid.UUID) (Exposure, error) {
	var e Exposure
	err := q.db.QueryRow(ctx, `
		SELECT
		  coalesce((SELECT sum(i.principal_due - i.principal_paid)
		              FROM lending.loans l JOIN lending.instalments i ON i.loan_id = l.loan_id AND i.schedule_version = l.schedule_version
		             WHERE l.customer_id = $1 AND l.state IN ('PENDING_DISBURSEMENT','ACTIVE','IN_ARREARS')), 0)::bigint,
		  EXISTS (SELECT 1 FROM lending.loans WHERE customer_id = $1 AND state IN ('PENDING_DISBURSEMENT','ACTIVE','IN_ARREARS')),
		  EXISTS (SELECT 1 FROM lending.loans WHERE customer_id = $1 AND written_off_minor > 0)`, customerID,
	).Scan(&e.OutstandingPrincipalMinor, &e.HasActiveLoan, &e.HasPriorWriteOff)
	return e, err
}

// ServicingLoanIDs pages through loans being serviced, by id, for the daily
// jobs. Keyset pagination keeps each page cheap however many loans exist.
func (q *Queries) ServicingLoanIDs(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT loan_id FROM lending.loans
		 WHERE state IN ('ACTIVE','IN_ARREARS') AND loan_id > $1
		 ORDER BY loan_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// AccruableLoanIDs pages through loans that may still have interest to
// recognise: loans being serviced, and loans that left servicing with a
// recognition lag (accrual_catchup).
func (q *Queries) AccruableLoanIDs(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT loan_id FROM lending.loans
		 WHERE loan_id > $1 AND (state IN ('ACTIVE','IN_ARREARS') OR accrual_catchup)
		 ORDER BY loan_id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ClaimCollection takes the collection lease on one loan: it returns the
// loan and pushes next_collection_at forward so no other worker collects it
// meanwhile. It also counts attempts per business date.
func (q *Queries) ClaimCollection(ctx context.Context, id uuid.UUID, now time.Time, today time.Time, retryAt time.Time) (attemptsToday int, ok bool, err error) {
	err = q.db.QueryRow(ctx, `
		UPDATE lending.loans
		   SET next_collection_at = $4,
		       collection_attempts = CASE WHEN collection_attempt_date = $3 THEN collection_attempts + 1 ELSE 1 END,
		       collection_attempt_date = $3
		 WHERE loan_id = $1 AND state IN ('ACTIVE','IN_ARREARS') AND auto_debit_authorised
		   AND next_collection_at IS NOT NULL AND next_collection_at <= $2
		RETURNING collection_attempts`, id, now, today, retryAt).Scan(&attemptsToday)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	return attemptsToday, err == nil, err
}

// LoansDueForCollection lists loans whose scheduled collection is due.
func (q *Queries) LoansDueForCollection(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT loan_id FROM lending.loans
		 WHERE next_collection_at <= $1 AND state IN ('ACTIVE','IN_ARREARS') AND auto_debit_authorised
		 ORDER BY next_collection_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ScheduleCollection sets when the loan should next be collected.
func (q *Queries) ScheduleCollection(ctx context.Context, id uuid.UUID, at *time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE lending.loans SET next_collection_at = $2 WHERE loan_id = $1`, id, at)
	return err
}

// WakeCollectionsForCustomer makes the customer's overdue auto-debit loans
// due for collection now. It is called when money arrives in the customer's
// account. It returns how many loans were woken.
func (q *Queries) WakeCollectionsForCustomer(ctx context.Context, customerID uuid.UUID, now time.Time) (int64, error) {
	tag, err := q.db.Exec(ctx, `
		UPDATE lending.loans SET next_collection_at = $2
		 WHERE customer_id = $1 AND state = 'IN_ARREARS' AND auto_debit_authorised
		   AND (next_collection_at IS NULL OR next_collection_at > $2)`, customerID, now)
	return tag.RowsAffected(), err
}

// LoansForCustomer lists a customer's loans, newest first.
func (q *Queries) LoansForCustomer(ctx context.Context, customerID uuid.UUID, limit int) ([]domain.Loan, error) {
	rows, err := q.db.Query(ctx, `SELECT `+loanColumns+` FROM lending.loans WHERE customer_id = $1 ORDER BY created_at DESC LIMIT $2`, customerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Loan
	for rows.Next() {
		l, err := scanLoan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ScheduleCollectionIfUnset sets the next collection time only when none is
// scheduled, so the daily job does not push back a retry that is already due.
func (q *Queries) ScheduleCollectionIfUnset(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := q.db.Exec(ctx, `UPDATE lending.loans SET next_collection_at = $2 WHERE loan_id = $1 AND next_collection_at IS NULL`, id, at)
	return err
}
