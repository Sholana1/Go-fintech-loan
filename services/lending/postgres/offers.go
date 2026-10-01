package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/services/lending/domain"
)

const offerColumns = `offer_id, application_id, decision_id, customer_id, principal_minor, tenor_months, monthly_rate_bps,
	origination_fee_minor, net_disbursement_minor, instalment_minor, total_interest_minor, total_repayable_minor,
	nominal_annual_rate_bps, effective_annual_cost_bps, disclosure, disclosure_hash, state, expires_at, accepted_at, created_at`

func scanOffer(row pgx.Row) (domain.Offer, error) {
	var (
		o     domain.Offer
		state string
	)
	err := row.Scan(&o.ID, &o.ApplicationID, &o.DecisionID, &o.CustomerID, &o.PrincipalMinor, &o.TenorMonths, &o.MonthlyRateBps,
		&o.OriginationFeeMinor, &o.NetDisbursementMinor, &o.InstalmentMinor, &o.TotalInterestMinor, &o.TotalRepayableMinor,
		&o.NominalAnnualRateBps, &o.EffectiveAnnualCostBps, &o.Disclosure, &o.DisclosureHash, &state, &o.ExpiresAt, &o.AcceptedAt, &o.CreatedAt)
	o.State = domain.OfferState(state)
	return o, err
}

// InsertOffer stores a new offer. The unique index on live offers means an
// application can have only one OFFERED offer at a time.
func (q *Queries) InsertOffer(ctx context.Context, o domain.Offer) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO lending.offers (`+offerColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		o.ID, o.ApplicationID, o.DecisionID, o.CustomerID, o.PrincipalMinor, o.TenorMonths, o.MonthlyRateBps,
		o.OriginationFeeMinor, o.NetDisbursementMinor, o.InstalmentMinor, o.TotalInterestMinor, o.TotalRepayableMinor,
		o.NominalAnnualRateBps, o.EffectiveAnnualCostBps, o.Disclosure, o.DisclosureHash, string(o.State), o.ExpiresAt, o.AcceptedAt, o.CreatedAt)
	return err
}

func (q *Queries) GetOfferForCustomer(ctx context.Context, id, customerID uuid.UUID) (domain.Offer, error) {
	o, err := scanOffer(q.db.QueryRow(ctx, `SELECT `+offerColumns+` FROM lending.offers WHERE offer_id = $1 AND customer_id = $2`, id, customerID))
	return o, notFound(err, "offer")
}

// LiveOffer returns the application's open offer, if any.
func (q *Queries) LiveOffer(ctx context.Context, applicationID uuid.UUID) (domain.Offer, error) {
	o, err := scanOffer(q.db.QueryRow(ctx, `SELECT `+offerColumns+` FROM lending.offers WHERE application_id = $1 AND state = 'OFFERED'`, applicationID))
	return o, notFound(err, "offer")
}

// LatestOffer returns the application's most recent offer in any state.
func (q *Queries) LatestOffer(ctx context.Context, applicationID uuid.UUID) (domain.Offer, error) {
	o, err := scanOffer(q.db.QueryRow(ctx, `
		SELECT `+offerColumns+` FROM lending.offers WHERE application_id = $1 ORDER BY created_at DESC LIMIT 1`, applicationID))
	return o, notFound(err, "offer")
}

// AcceptOffer marks the offer accepted if, and only if, it is still open and
// unexpired at `now`. This single statement is what makes concurrent
// acceptance safe: of any number of simultaneous attempts exactly one
// updates the row.
func (q *Queries) AcceptOffer(ctx context.Context, id uuid.UUID, now time.Time, acceptance map[string]any) (bool, error) {
	raw, err := json.Marshal(acceptance)
	if err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `
		UPDATE lending.offers SET state = 'ACCEPTED', accepted_at = $2, acceptance = $3
		 WHERE offer_id = $1 AND state = 'OFFERED' AND expires_at > $2`, id, now, raw)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CloseLiveOffer moves the application's open offer to EXPIRED or VOIDED.
func (q *Queries) CloseLiveOffer(ctx context.Context, applicationID uuid.UUID, to domain.OfferState) error {
	_, err := q.db.Exec(ctx, `UPDATE lending.offers SET state = $2 WHERE application_id = $1 AND state = 'OFFERED'`, applicationID, string(to))
	return err
}

// ExpiredOpenOffers lists applications whose open offer has passed its expiry.
func (q *Queries) ExpiredOpenOffers(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT application_id FROM lending.offers WHERE state = 'OFFERED' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
