package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// recordDecision writes the decision snapshot and moves the application, in
// one transaction. For an approval it also creates the offer.
func (s *Service) recordDecision(ctx context.Context, a domain.Application, features domain.Features, refs map[string]string, d domain.Decision) error {
	now := s.now()
	snapshot := domain.DecisionSnapshot{
		ID: uuid.New(), ApplicationID: a.ID, Decision: d, PolicyVersion: s.policy.Version,
		ProductID: s.product.ID, ProductVersion: s.product.Version, Features: features, InputRefs: refs,
		DecidedBy: "AUTO", CreatedAt: now,
	}
	var applied bool
	err := s.store.InTx(ctx, func(q *postgres.Queries) error {
		var err error
		applied, err = s.applyDecision(ctx, q, a, snapshot, []domain.ApplicationState{domain.AppAssessing}, postgres.SystemActor, now)
		return err
	})
	if err != nil {
		return err
	}
	if applied && d.Outcome != domain.OutcomeRefer {
		s.metrics.ApplicationDecided(string(d.Outcome), now.Sub(a.SubmittedAt))
	}
	return nil
}

// applyDecision is shared by automated assessment and manual review. It
// returns false when the application had already left the expected states
// (for example the customer cancelled while the assessment was running), in
// which case nothing is written.
func (s *Service) applyDecision(ctx context.Context, q *postgres.Queries, a domain.Application, snap domain.DecisionSnapshot, from []domain.ApplicationState, actor postgres.Actor, now time.Time) (bool, error) {
	d := snap.Decision
	update := postgres.ApplicationUpdate{Reasons: d.ReasonCodes}
	var (
		offer     *domain.Offer
		eventType string
	)
	switch d.Outcome {
	case domain.OutcomeDecline:
		update.To, update.DecidedAt, eventType = domain.AppDeclined, &now, "loan.application.declined"
	case domain.OutcomeRefer:
		// T1 is not set: the decision is still to come, from a reviewer.
		update.To, eventType = domain.AppReferred, "loan.application.referred"
	case domain.OutcomeApprove:
		o, err := s.buildOffer(a, snap, now)
		if err != nil {
			return false, err
		}
		offer = &o
		update.To, update.DecidedAt, eventType = domain.AppOffered, &now, "loan.application.offered"
	default:
		return false, fmt.Errorf("unknown decision outcome %q", d.Outcome)
	}

	ok, err := q.TransitionApplication(ctx, a.ID, from, update, now)
	if err != nil || !ok {
		return false, err
	}
	if err := q.InsertDecision(ctx, snap); err != nil {
		return false, err
	}
	offerID := ""
	if offer != nil {
		if err := q.InsertOffer(ctx, *offer); err != nil {
			return false, err
		}
		offerID = offer.ID.String()
	}
	if err := q.Audit(ctx, actor, "APPLICATION_"+string(d.Outcome), "loan_application", a.ID.String(),
		map[string]any{"decision_id": snap.ID.String(), "reason_codes": d.ReasonCodes, "policy_version": snap.PolicyVersion, "offer_id": offerID}); err != nil {
		return false, err
	}
	return true, s.emitApplication(ctx, q, eventType, a, update.To, d.ReasonCodes, offerID, a.Version+1)
}

// buildOffer prices an approved decision into an immutable offer.
func (s *Service) buildOffer(a domain.Application, snap domain.DecisionSnapshot, now time.Time) (domain.Offer, error) {
	d := snap.Decision
	terms, err := domain.PriceOffer(s.product, d.ApprovedPrincipalMinor, a.TenorMonths, d.MonthlyRateBps, s.today())
	if err != nil {
		return domain.Offer{}, err
	}
	expires := now.Add(s.product.OfferValidity())
	doc, hash, err := domain.BuildDisclosure(s.product, terms, expires).Canonical()
	if err != nil {
		return domain.Offer{}, err
	}
	return domain.Offer{
		ID: uuid.New(), ApplicationID: a.ID, DecisionID: snap.ID, CustomerID: a.CustomerID,
		PrincipalMinor: terms.PrincipalMinor, TenorMonths: terms.TenorMonths, MonthlyRateBps: terms.MonthlyRateBps,
		OriginationFeeMinor: terms.OriginationFeeMinor, NetDisbursementMinor: terms.NetDisbursementMinor,
		InstalmentMinor: terms.InstalmentMinor, TotalInterestMinor: terms.TotalInterestMinor, TotalRepayableMinor: terms.TotalRepayableMinor,
		NominalAnnualRateBps: terms.NominalAnnualRateBps, EffectiveAnnualCostBps: terms.EffectiveAnnualCostBps,
		Disclosure: doc, DisclosureHash: hash, State: domain.OfferOpen, ExpiresAt: expires.UTC().Truncate(time.Second), CreatedAt: now,
	}, nil
}
