package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
)

// InsertBureauReport stores a raw bureau response. A repeat of the same
// request reference is a no-op (the provider returned the same report).
func (q *Queries) InsertBureauReport(ctx context.Context, applicationID, customerID uuid.UUID, r domain.BureauReport, rawSHA256 string) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO lending.bureau_reports (report_id, application_id, customer_id, provider, request_ref, provider_ref, fetched_at, raw, raw_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (provider, request_ref) DO NOTHING`,
		r.ID, applicationID, customerID, r.Provider, r.RequestRef, r.ProviderRef, r.FetchedAt, r.Raw, rawSHA256)
	return err
}

// InsertDecision stores an immutable decision snapshot.
func (q *Queries) InsertDecision(ctx context.Context, d domain.DecisionSnapshot) error {
	features, err := json.Marshal(d.Features)
	if err != nil {
		return err
	}
	refs, err := json.Marshal(d.InputRefs)
	if err != nil {
		return err
	}
	reasons := d.Decision.ReasonCodes
	if reasons == nil {
		reasons = []string{}
	}
	_, err = q.db.Exec(ctx, `
		INSERT INTO lending.decision_snapshots (decision_id, application_id, outcome, reason_codes, approved_principal_minor,
			risk_band, monthly_rate_bps, policy_version, product_id, product_version, features, input_refs, decided_by, reviewer_note, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,nullif($14,''),$15)`,
		d.ID, d.ApplicationID, string(d.Decision.Outcome), reasons, d.Decision.ApprovedPrincipalMinor,
		d.Decision.RiskBand, d.Decision.MonthlyRateBps, d.PolicyVersion, d.ProductID, d.ProductVersion,
		features, refs, d.DecidedBy, d.ReviewerNote, d.CreatedAt)
	return err
}

// Decisions returns an application's decision history, oldest first.
func (q *Queries) Decisions(ctx context.Context, applicationID uuid.UUID) ([]domain.DecisionSnapshot, error) {
	rows, err := q.db.Query(ctx, `
		SELECT decision_id, application_id, outcome, reason_codes, approved_principal_minor, risk_band, monthly_rate_bps,
		       policy_version, product_id, product_version, features, input_refs, decided_by, coalesce(reviewer_note,''), created_at
		  FROM lending.decision_snapshots WHERE application_id = $1 ORDER BY created_at`, applicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DecisionSnapshot
	for rows.Next() {
		var (
			d              domain.DecisionSnapshot
			outcome        string
			features, refs []byte
		)
		if err := rows.Scan(&d.ID, &d.ApplicationID, &outcome, &d.Decision.ReasonCodes, &d.Decision.ApprovedPrincipalMinor,
			&d.Decision.RiskBand, &d.Decision.MonthlyRateBps, &d.PolicyVersion, &d.ProductID, &d.ProductVersion,
			&features, &refs, &d.DecidedBy, &d.ReviewerNote, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.Decision.Outcome = domain.Outcome(outcome)
		if err := json.Unmarshal(features, &d.Features); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(refs, &d.InputRefs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
