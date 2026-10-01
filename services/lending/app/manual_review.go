package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// Staff operations. Authorisation by role happens in the HTTP layer; the
// rules that protect money (reviewers bound by policy sizing, maker and
// checker being different people) are enforced here and, for maker-checker,
// again by a CHECK constraint in the database.

// ReviewItem is one entry of the manual-review queue.
type ReviewItem struct {
	Application domain.Application
	Decisions   []domain.DecisionSnapshot
}

// ManualReviewQueue lists referred applications, oldest first, each with the
// decision snapshot that referred it. The snapshot holds features and reason
// codes, not raw identity data.
func (s *Service) ManualReviewQueue(ctx context.Context) ([]ReviewItem, error) {
	apps, err := s.store.ReferredApplications(ctx, 100)
	if err != nil {
		return nil, err
	}
	out := make([]ReviewItem, 0, len(apps))
	for _, a := range apps {
		ds, err := s.store.Decisions(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ReviewItem{Application: a, Decisions: ds})
	}
	return out, nil
}

// ReviewDecision is a credit reviewer's decision on a referred application.
type ReviewDecision struct {
	Approve bool `json:"approve"`
	// ApprovedPrincipalMinor and RiskBand are required for an approval.
	ApprovedPrincipalMinor int64  `json:"approved_principal_minor"`
	RiskBand               string `json:"risk_band"`
	Note                   string `json:"note"`
}

// DecideManualReview records a reviewer's decision on a REFERRED application
// and, for an approval, creates the offer.
//
// A reviewer settles the reason for referral (a thin file, a fraud review, an
// amount above the auto-approval ceiling). A reviewer cannot exceed what the
// policy's sizing rules allow for this customer: the same SizeLoan function
// bounds both the automated and the manual path. The decision is stored as a
// new immutable snapshot carrying the reviewer's id and note.
func (s *Service) DecideManualReview(ctx context.Context, reviewerID uuid.UUID, applicationID uuid.UUID, in ReviewDecision) (ApplicationView, error) {
	if len(strings.TrimSpace(in.Note)) < 10 {
		return ApplicationView{}, fmt.Errorf("%w: a review note of at least 10 characters is required", domain.ErrValidation)
	}
	a, err := s.store.GetApplication(ctx, applicationID)
	if err != nil {
		return ApplicationView{}, err
	}
	if a.State != domain.AppReferred {
		return ApplicationView{}, fmt.Errorf("%w: application is %s, not REFERRED", domain.ErrConflict, a.State)
	}
	history, err := s.store.Decisions(ctx, a.ID)
	if err != nil {
		return ApplicationView{}, err
	}
	if len(history) == 0 {
		return ApplicationView{}, fmt.Errorf("referred application %s has no decision snapshot", a.ID)
	}
	referral := history[len(history)-1]

	decision := domain.Decision{Outcome: domain.OutcomeDecline, ReasonCodes: []string{domain.ReasonManualDecline}}
	if in.Approve {
		rateBps, ok := s.product.RateFor(in.RiskBand)
		if !ok {
			return ApplicationView{}, fmt.Errorf("%w: unknown risk band", domain.ErrValidation)
		}
		// Hard exclusions cannot be reviewed away.
		if reasons := domain.Ineligible(s.product, referral.Features); len(reasons) > 0 {
			return ApplicationView{}, fmt.Errorf("%w: application is ineligible (%s)", domain.ErrForbidden, strings.Join(reasons, ","))
		}
		limit, _, declined := domain.SizeLoan(s.policy, s.product, referral.Features, rateBps)
		if declined != "" {
			return ApplicationView{}, fmt.Errorf("%w: policy does not permit any amount (%s)", domain.ErrForbidden, declined)
		}
		p := s.product
		switch {
		case in.ApprovedPrincipalMinor < p.MinPrincipalMinor || in.ApprovedPrincipalMinor%p.PrincipalStepMinor != 0:
			return ApplicationView{}, fmt.Errorf("%w: approved amount must be at least the product minimum and a whole step", domain.ErrValidation)
		case in.ApprovedPrincipalMinor > limit:
			return ApplicationView{}, fmt.Errorf("%w: approved amount exceeds the policy limit of %d for this customer", domain.ErrForbidden, limit)
		}
		decision = domain.Decision{
			Outcome: domain.OutcomeApprove, ReasonCodes: []string{domain.ReasonManualApproval},
			ApprovedPrincipalMinor: in.ApprovedPrincipalMinor, RiskBand: in.RiskBand, MonthlyRateBps: rateBps,
		}
	}

	now := s.now()
	snapshot := domain.DecisionSnapshot{
		ID: uuid.New(), ApplicationID: a.ID, Decision: decision, PolicyVersion: s.policy.Version,
		ProductID: s.product.ID, ProductVersion: s.product.Version,
		Features: referral.Features, InputRefs: map[string]string{"reviewed_decision": referral.ID.String()},
		DecidedBy: reviewerID.String(), ReviewerNote: strings.TrimSpace(in.Note), CreatedAt: now,
	}
	var applied bool
	err = s.store.InTx(ctx, func(q *postgres.Queries) error {
		var err error
		applied, err = s.applyDecision(ctx, q, a, snapshot, []domain.ApplicationState{domain.AppReferred},
			postgres.Actor{Kind: "staff", ID: reviewerID.String()}, now)
		return err
	})
	if err != nil {
		return ApplicationView{}, err
	}
	if !applied {
		return ApplicationView{}, fmt.Errorf("%w: the application was decided or cancelled by someone else", domain.ErrConflict)
	}
	s.metrics.ApplicationDecided("MANUAL_"+string(decision.Outcome), now.Sub(a.SubmittedAt))
	a, err = s.store.GetApplication(ctx, a.ID)
	if err != nil {
		return ApplicationView{}, err
	}
	return s.viewOf(ctx, a)
}
