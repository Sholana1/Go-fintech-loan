package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/services/lending/domain"
)

const externalPaymentColumns = `payment_id, loan_id, customer_id, reference, provider, provider_ref, expected_minor,
	verified_minor, currency, state, last_code, journal_id, repayment_id, attempts, expires_at, state_changed_at, created_at`

func scanExternalPayment(row pgx.Row) (domain.ExternalPayment, error) {
	var (
		p     domain.ExternalPayment
		state string
	)
	err := row.Scan(&p.ID, &p.LoanID, &p.CustomerID, &p.Reference, &p.Provider, &p.ProviderRef, &p.ExpectedMinor,
		&p.VerifiedMinor, &p.Currency, &state, &p.LastCode, &p.JournalID, &p.RepaymentID, &p.Attempts, &p.ExpiresAt, &p.StateChanged, &p.CreatedAt)
	p.State = domain.ExternalPaymentState(state)
	return p, err
}

// InsertExternalPayment stores a payment the customer is about to make. The
// reference is fixed here, before the customer is sent to the provider.
func (q *Queries) InsertExternalPayment(ctx context.Context, p domain.ExternalPayment, firstCheckAt time.Time) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO lending.external_payments (payment_id, loan_id, customer_id, reference, provider, expected_minor,
			currency, state, next_attempt_at, expires_at, state_changed_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)`,
		p.ID, p.LoanID, p.CustomerID, p.Reference, p.Provider, p.ExpectedMinor, p.Currency, string(p.State),
		firstCheckAt, p.ExpiresAt, p.CreatedAt)
	return err
}

func (q *Queries) GetExternalPayment(ctx context.Context, id uuid.UUID) (domain.ExternalPayment, error) {
	p, err := scanExternalPayment(q.db.QueryRow(ctx, `SELECT `+externalPaymentColumns+` FROM lending.external_payments WHERE payment_id = $1`, id))
	return p, notFound(err, "external payment")
}

// GetExternalPaymentForCustomer returns the payment only if it belongs to
// the customer and the loan. A foreign id is indistinguishable from a
// missing one.
func (q *Queries) GetExternalPaymentForCustomer(ctx context.Context, id, loanID, customerID uuid.UUID) (domain.ExternalPayment, error) {
	p, err := scanExternalPayment(q.db.QueryRow(ctx, `
		SELECT `+externalPaymentColumns+` FROM lending.external_payments
		 WHERE payment_id = $1 AND loan_id = $2 AND customer_id = $3`, id, loanID, customerID))
	return p, notFound(err, "external payment")
}

func (q *Queries) ExternalPaymentByReference(ctx context.Context, reference string) (domain.ExternalPayment, error) {
	p, err := scanExternalPayment(q.db.QueryRow(ctx, `SELECT `+externalPaymentColumns+` FROM lending.external_payments WHERE reference = $1`, reference))
	return p, notFound(err, "external payment")
}

// ExternalPaymentUpdate describes a payment state change. Empty and nil
// fields keep the stored value, except NextAttemptAt: nil clears the
// work-queue entry.
type ExternalPaymentUpdate struct {
	To            domain.ExternalPaymentState
	ProviderRef   string
	Code          string
	VerifiedMinor *int64
	JournalID     *int64
	RepaymentID   *uuid.UUID
	NextAttemptAt *time.Time
	CountAttempt  bool
}

// TransitionExternalPayment moves a payment from `from` to u.To,
// compare-and-set. It returns false if the payment was not in `from`: a
// webhook and the poller raced and the other one moved it first. The loser
// stops, so a payment is verified, credited and applied once.
func (q *Queries) TransitionExternalPayment(ctx context.Context, id uuid.UUID, from domain.ExternalPaymentState, u ExternalPaymentUpdate, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
		UPDATE lending.external_payments
		   SET state = $3,
		       provider_ref = CASE WHEN $4 = '' THEN provider_ref ELSE $4 END,
		       last_code = CASE WHEN $5 = '' THEN last_code ELSE $5 END,
		       verified_minor = coalesce($6, verified_minor),
		       journal_id = coalesce($7, journal_id),
		       repayment_id = coalesce($8, repayment_id),
		       next_attempt_at = $9,
		       attempts = attempts + CASE WHEN $10 THEN 1 ELSE 0 END,
		       state_changed_at = CASE WHEN state = $3 THEN state_changed_at ELSE $11 END,
		       version = version + 1
		 WHERE payment_id = $1 AND state = $2`,
		id, string(from), string(u.To), u.ProviderRef, u.Code, u.VerifiedMinor, u.JournalID, u.RepaymentID,
		u.NextAttemptAt, u.CountAttempt, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimExternalPayment takes the driver lease on a payment whose next
// attempt is due, pushing next_attempt_at past the lease.
func (q *Queries) ClaimExternalPayment(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (domain.ExternalPayment, bool, error) {
	p, err := scanExternalPayment(q.db.QueryRow(ctx, `
		UPDATE lending.external_payments SET next_attempt_at = $3
		 WHERE payment_id = $1 AND next_attempt_at IS NOT NULL AND next_attempt_at <= $2
		RETURNING `+externalPaymentColumns, id, now, now.Add(lease)))
	if err == pgx.ErrNoRows {
		return domain.ExternalPayment{}, false, nil
	}
	return p, err == nil, err
}

// WakeExternalPayment makes a payment due now: a webhook says the provider
// has news. A payment still waiting for the provider is checked at once; a
// payment we had given up on (FAILED, EXPIRED) is put back on the work queue
// so a late success is recognised. A payment already past verification is
// left to its own schedule. Waking a payment another worker is driving is
// safe: every step is a compare-and-set and the ledger credit is idempotent.
func (q *Queries) WakeExternalPayment(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := q.db.Exec(ctx, `
		UPDATE lending.external_payments SET next_attempt_at = $2
		 WHERE payment_id = $1 AND (next_attempt_at IS NULL OR state = 'INITIATED')`, id, now)
	return err
}

// DueExternalPayments lists payments whose next attempt is due.
func (q *Queries) DueExternalPayments(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT payment_id FROM lending.external_payments WHERE next_attempt_at <= $1 ORDER BY next_attempt_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// RecordExternalPaymentEvent stores a provider webhook. It returns false if
// the event id was already recorded (a duplicate delivery).
func (q *Queries) RecordExternalPaymentEvent(ctx context.Context, provider, eventID, reference string, raw []byte) (bool, error) {
	if raw == nil {
		raw = []byte{}
	}
	tag, err := q.db.Exec(ctx, `
		INSERT INTO lending.external_payment_events (provider, event_id, reference, raw) VALUES ($1,$2,$3,$4)
		ON CONFLICT (provider, event_id) DO NOTHING`, provider, eventID, reference, raw)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CollectionsTotals is lending's view of the collections clearing account.
type CollectionsTotals struct {
	// CreditedMinor is the sum of provider-confirmed payments whose ledger
	// credit has been recorded.
	CreditedMinor int64
	// InFlight is the number of confirmed payments whose credit may or may
	// not have reached the ledger yet.
	InFlight int
}

func (q *Queries) CollectionsTotals(ctx context.Context) (CollectionsTotals, error) {
	var t CollectionsTotals
	err := q.db.QueryRow(ctx, `
		SELECT coalesce(sum(verified_minor) FILTER (WHERE state IN ('CREDITED','APPLIED','UNAPPLIED')), 0)::bigint,
		       count(*) FILTER (WHERE state = 'VERIFIED')
		  FROM lending.external_payments`).Scan(&t.CreditedMinor, &t.InFlight)
	return t, err
}
