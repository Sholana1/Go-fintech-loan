package app

import (
	"context"
	"errors"
	"time"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

func (s *Service) expireApplication(ctx context.Context, a domain.Application, now time.Time) error {
	reasons := []string{domain.ReasonApplicationExpired}
	if a.WaitingOn == "CREDIT_BUREAU" {
		reasons = append(reasons, domain.ReasonBureauUnavailable)
	}
	return s.store.InTx(ctx, func(q *postgres.Queries) error {
		ok, err := q.TransitionApplication(ctx, a.ID,
			[]domain.ApplicationState{domain.AppSubmitted, domain.AppAssessing, domain.AppReferred},
			postgres.ApplicationUpdate{To: domain.AppExpired, Reasons: reasons}, now)
		if err != nil || !ok {
			return err
		}
		if err := q.Audit(ctx, postgres.SystemActor, "APPLICATION_EXPIRED", "loan_application", a.ID.String(), map[string]any{"waiting_on": a.WaitingOn}); err != nil {
			return err
		}
		return s.emitApplication(ctx, q, "loan.application.expired", a, domain.AppExpired, reasons, "", a.Version+1)
	})
}

// ExpireStale expires applications and offers past their validity. It runs
// frequently; each step is a compare-and-set, so it is safe to run from
// several instances.
func (s *Service) ExpireStale(ctx context.Context) error {
	now := s.now()
	var errs []error

	apps, err := s.store.ExpirableApplications(ctx, now, 200)
	if err != nil {
		return err
	}
	for _, id := range apps {
		a, err := s.store.GetApplication(ctx, id)
		if err == nil {
			err = s.expireApplication(ctx, a, now)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	offers, err := s.store.ExpiredOpenOffers(ctx, now, 200)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, applicationID := range offers {
		a, err := s.store.GetApplication(ctx, applicationID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = s.store.InTx(ctx, func(q *postgres.Queries) error {
			ok, err := q.TransitionApplication(ctx, a.ID, []domain.ApplicationState{domain.AppOffered},
				postgres.ApplicationUpdate{To: domain.AppExpired, Reasons: []string{domain.ReasonOfferExpired}}, now)
			if err != nil || !ok {
				return err // not OFFERED any more: it was accepted or cancelled first
			}
			if err := q.CloseLiveOffer(ctx, a.ID, domain.OfferExpired); err != nil {
				return err
			}
			return s.emitApplication(ctx, q, "loan.application.expired", a, domain.AppExpired, []string{domain.ReasonOfferExpired}, "", a.Version+1)
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := s.store.DeleteExpiredIdempotencyKeys(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
