package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/services/lending/domain"
)

const payoutColumns = `payout_id, loan_id, customer_id, amount_minor, bank_code, account_number, account_name,
	reference, provider_ref, state, last_code, attempts, state_changed_at, created_at`

func scanPayout(row pgx.Row) (domain.Payout, error) {
	var (
		p     domain.Payout
		state string
	)
	err := row.Scan(&p.ID, &p.LoanID, &p.CustomerID, &p.AmountMinor, &p.BankCode, &p.AccountNumber, &p.AccountName,
		&p.Reference, &p.ProviderRef, &state, &p.LastCode, &p.Attempts, &p.StateChanged, &p.CreatedAt)
	p.State = domain.PayoutState(state)
	return p, err
}

// InsertPayout stores a payout instruction. The reference is fixed here,
// before any provider call is made.
func (q *Queries) InsertPayout(ctx context.Context, p domain.Payout, provider, enquiryRef string) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO lending.payouts (payout_id, loan_id, customer_id, amount_minor, bank_code, account_number, account_name,
			enquiry_ref, reference, provider, state, state_changed_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)`,
		p.ID, p.LoanID, p.CustomerID, p.AmountMinor, p.BankCode, p.AccountNumber, p.AccountName,
		enquiryRef, p.Reference, provider, string(p.State), p.CreatedAt)
	return err
}

func (q *Queries) GetPayout(ctx context.Context, id uuid.UUID) (domain.Payout, error) {
	p, err := scanPayout(q.db.QueryRow(ctx, `SELECT `+payoutColumns+` FROM lending.payouts WHERE payout_id = $1`, id))
	return p, notFound(err, "payout")
}

func (q *Queries) PayoutByLoan(ctx context.Context, loanID uuid.UUID) (domain.Payout, error) {
	p, err := scanPayout(q.db.QueryRow(ctx, `SELECT `+payoutColumns+` FROM lending.payouts WHERE loan_id = $1`, loanID))
	return p, notFound(err, "payout")
}

func (q *Queries) PayoutByReference(ctx context.Context, reference string) (domain.Payout, error) {
	p, err := scanPayout(q.db.QueryRow(ctx, `SELECT `+payoutColumns+` FROM lending.payouts WHERE reference = $1`, reference))
	return p, notFound(err, "payout")
}

// PayoutUpdate describes a payout state change.
type PayoutUpdate struct {
	To            domain.PayoutState
	ProviderRef   string     // kept if empty
	Code          string     // kept if empty
	NextAttemptAt *time.Time // nil clears the work-queue entry
	CountAttempt  bool
}

// TransitionPayout moves a payout from `from` to u.To, compare-and-set. It
// returns false if the payout was not in `from`: another worker, a callback
// or the reconciler moved it first, and the caller must re-read and decide
// again. This is what stops two paths from both sending, or one path from
// releasing funds that another has just captured.
func (q *Queries) TransitionPayout(ctx context.Context, id uuid.UUID, from domain.PayoutState, u PayoutUpdate, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
		UPDATE lending.payouts
		   SET state = $3,
		       provider_ref = CASE WHEN $4 = '' THEN provider_ref ELSE $4 END,
		       last_code = CASE WHEN $5 = '' THEN last_code ELSE $5 END,
		       next_attempt_at = $6,
		       attempts = attempts + CASE WHEN $7 THEN 1 ELSE 0 END,
		       state_changed_at = CASE WHEN state = $3 THEN state_changed_at ELSE $8 END,
		       version = version + 1
		 WHERE payout_id = $1 AND state = $2`,
		id, string(from), string(u.To), u.ProviderRef, u.Code, u.NextAttemptAt, u.CountAttempt, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimPayout takes the driver lease on a payout whose next attempt is due,
// pushing next_attempt_at past the lease, and returns the payout.
func (q *Queries) ClaimPayout(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (domain.Payout, bool, error) {
	p, err := scanPayout(q.db.QueryRow(ctx, `
		UPDATE lending.payouts SET next_attempt_at = $3
		 WHERE payout_id = $1 AND next_attempt_at IS NOT NULL AND next_attempt_at <= $2
		RETURNING `+payoutColumns, id, now, now.Add(lease)))
	if err == pgx.ErrNoRows {
		return domain.Payout{}, false, nil
	}
	return p, err == nil, err
}

// DuePayouts lists payouts whose next attempt is due.
func (q *Queries) DuePayouts(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT payout_id FROM lending.payouts WHERE next_attempt_at <= $1 ORDER BY next_attempt_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// RecordPayoutEvent stores a provider callback. It returns false if the event
// id was already recorded: the callback is a duplicate and must not be
// applied again.
func (q *Queries) RecordPayoutEvent(ctx context.Context, provider, eventID, reference, outcome string, raw []byte) (bool, error) {
	if raw == nil {
		raw = []byte{}
	}
	tag, err := q.db.Exec(ctx, `
		INSERT INTO lending.payout_events (provider, event_id, reference, outcome, raw) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (provider, event_id) DO NOTHING`, provider, eventID, reference, outcome, raw)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// PayoutsInStates lists payouts in the given states created up to `before`,
// for reconciliation.
func (q *Queries) PayoutsInStates(ctx context.Context, states []domain.PayoutState, before time.Time, limit int) ([]domain.Payout, error) {
	s := make([]string, len(states))
	for i, st := range states {
		s[i] = string(st)
	}
	rows, err := q.db.Query(ctx, `
		SELECT `+payoutColumns+` FROM lending.payouts WHERE state = ANY($1) AND created_at <= $2 ORDER BY created_at LIMIT $3`, s, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Payout
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
