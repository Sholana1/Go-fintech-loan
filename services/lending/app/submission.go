package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

const endpointSubmit = "POST /v1/loan-applications"

// SubmitRequest is a customer's loan application.
//
// It asks for the minimum the decision needs: amount, tenor and stated
// income. Identity comes from the customer's verified record, not from the
// request, and no device, contact or location data is collected.
type SubmitRequest struct {
	ProductID                string `json:"product_id"`
	AmountMinor              int64  `json:"amount_minor"`
	Currency                 string `json:"currency"`
	TenorMonths              int    `json:"tenor_months"`
	StatedMonthlyIncomeMinor int64  `json:"stated_monthly_income_minor"`
	// ConsentCreditCheck records the customer's consent to a credit-bureau
	// enquiry and to automated assessment with a right to human review.
	// Without it the application is not accepted.
	ConsentCreditCheck bool `json:"consent_credit_check"`
}

func (s *Service) validateSubmit(req SubmitRequest) error {
	p := s.product
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", domain.ErrValidation, fmt.Sprintf(format, a...))
	}
	switch {
	case req.ProductID != p.ID:
		return fail("unknown product")
	case req.Currency != string(p.Currency):
		return fail("currency must be %s", p.Currency)
	case req.AmountMinor < p.MinPrincipalMinor || req.AmountMinor > p.MaxPrincipalMinor:
		return fail("amount must be between %d and %d minor units", p.MinPrincipalMinor, p.MaxPrincipalMinor)
	case req.AmountMinor%p.PrincipalStepMinor != 0:
		return fail("amount must be a multiple of %d minor units", p.PrincipalStepMinor)
	case !p.AllowsTenor(req.TenorMonths):
		return fail("tenor must be one of %v months", p.TenorsMonths)
	case req.StatedMonthlyIncomeMinor <= 0:
		return fail("stated monthly income is required")
	case !req.ConsentCreditCheck:
		return fail("consent to a credit check is required to apply")
	}
	return nil
}

// SubmitApplication records a new application (T0) and queues it for
// assessment. It returns replay=true when the idempotency key had already
// been used with the same request, in which case the original application is
// returned and nothing new is created.
//
// Duplicate handling has two layers. The idempotency key makes a client
// retry safe. The partial unique index on open applications makes two
// different requests from the same customer safe: exactly one is inserted
// and the other receives ErrApplicationOpen.
func (s *Service) SubmitApplication(ctx context.Context, customerID uuid.UUID, idemKey string, req SubmitRequest) (app domain.Application, replay bool, err error) {
	if err := validateIdemKey(idemKey); err != nil {
		return app, false, err
	}
	if err := s.validateSubmit(req); err != nil {
		return app, false, err
	}
	hash, err := requestHash(req)
	if err != nil {
		return app, false, err
	}

	now := s.now()
	app = domain.Application{
		ID: uuid.New(), CustomerID: customerID, ProductID: s.product.ID, ProductVersion: s.product.Version,
		RequestedPrincipalMinor: req.AmountMinor, TenorMonths: req.TenorMonths, StatedMonthlyIncomeMinor: req.StatedMonthlyIncomeMinor,
		State: domain.AppSubmitted, ConsentRef: uuid.New(), SubmittedAt: now, ExpiresAt: now.Add(s.product.ApplicationValidity()),
	}

	var existing uuid.UUID
	err = s.store.InTx(ctx, func(q *postgres.Queries) error {
		id, claimed, err := q.ClaimIdempotencyKey(ctx, customerID, endpointSubmit, idemKey, hash, app.ID, now.Add(s.cfg.IdempotencyRetention))
		if err != nil {
			return err
		}
		if !claimed {
			existing = id
			return nil
		}
		if err := q.InsertApplication(ctx, app, s.cfg.ConsentPolicyVersion, now); err != nil {
			return err
		}
		if err := q.Audit(ctx, postgres.Actor{Kind: "customer", ID: customerID.String()}, "APPLICATION_SUBMITTED", "loan_application", app.ID.String(),
			map[string]any{"consent_ref": app.ConsentRef.String(), "consent_policy_version": s.cfg.ConsentPolicyVersion,
				"requested_minor": req.AmountMinor, "tenor_months": req.TenorMonths}); err != nil {
			return err
		}
		return s.emitApplication(ctx, q, "loan.application.submitted", app, domain.AppSubmitted, nil, "", 1)
	})
	if err != nil {
		return domain.Application{}, false, err
	}
	if existing != uuid.Nil {
		s.metrics.IdempotentReplay(endpointSubmit)
		app, err = s.store.GetApplicationForCustomer(ctx, existing, customerID)
		return app, true, err
	}

	// Hand the application to an assessment worker. If the queue is full the
	// sweeper finds it by next_attempt_at within its polling interval.
	select {
	case s.kick <- app.ID:
	default:
	}
	return app, false, nil
}

// ApplicationView is an application with its current offer and loan, if any.
type ApplicationView struct {
	Application domain.Application
	Offer       *domain.Offer
	LoanID      *uuid.UUID
}

// GetApplication returns the customer's own application. Another customer's
// id yields ErrNotFound, exactly as a non-existent id does.
func (s *Service) GetApplication(ctx context.Context, customerID, applicationID uuid.UUID) (ApplicationView, error) {
	a, err := s.store.GetApplicationForCustomer(ctx, applicationID, customerID)
	if err != nil {
		return ApplicationView{}, err
	}
	return s.viewOf(ctx, a)
}

func (s *Service) viewOf(ctx context.Context, a domain.Application) (ApplicationView, error) {
	v := ApplicationView{Application: a}
	if o, err := s.store.LatestOffer(ctx, a.ID); err == nil {
		v.Offer = &o
	} else if !errors.Is(err, domain.ErrNotFound) {
		return v, err
	}
	if l, err := s.store.LoanByApplication(ctx, a.ID); err == nil {
		v.LoanID = &l.ID
	} else if !errors.Is(err, domain.ErrNotFound) {
		return v, err
	}
	return v, nil
}

// CancelApplication withdraws an application that has not been accepted.
func (s *Service) CancelApplication(ctx context.Context, customerID, applicationID uuid.UUID) error {
	a, err := s.store.GetApplicationForCustomer(ctx, applicationID, customerID)
	if err != nil {
		return err
	}
	now := s.now()
	return s.store.InTx(ctx, func(q *postgres.Queries) error {
		ok, err := q.TransitionApplication(ctx, a.ID,
			[]domain.ApplicationState{domain.AppSubmitted, domain.AppAssessing, domain.AppReferred, domain.AppOffered},
			postgres.ApplicationUpdate{To: domain.AppCancelled}, now)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: the application can no longer be cancelled", domain.ErrConflict)
		}
		if err := q.CloseLiveOffer(ctx, a.ID, domain.OfferVoided); err != nil {
			return err
		}
		if err := q.Audit(ctx, postgres.Actor{Kind: "customer", ID: customerID.String()}, "APPLICATION_CANCELLED", "loan_application", a.ID.String(), nil); err != nil {
			return err
		}
		return s.emitApplication(ctx, q, "loan.application.cancelled", a, domain.AppCancelled, nil, "", a.Version+1)
	})
}
