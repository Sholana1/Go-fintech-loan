package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// opExternalPayment is the ledger posting-reference type for the credit of
// an external payment. With the payment id it identifies the journal, so
// posting any number of times credits the customer once.
const opExternalPayment = "LENDING_EXTERNAL_PAYMENT"

// DriveDueExternalPayments advances every external payment whose next
// attempt is due. It is both the poller (for payments whose webhook never
// came) and the recovery path (for payments interrupted by a crash).
func (s *Service) DriveDueExternalPayments(ctx context.Context) error {
	if s.collect == nil {
		return nil
	}
	ids, err := s.store.DueExternalPayments(ctx, s.now(), 50)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := s.DriveExternalPayment(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("external payment %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// DriveExternalPayment advances one payment by as many steps as it can. The
// lease keeps workers from duplicating provider calls; correctness does not
// depend on it, because every state change is a compare-and-set and both
// money-moving steps are idempotent on the payment id.
func (s *Service) DriveExternalPayment(ctx context.Context, paymentID uuid.UUID) error {
	if s.collect == nil {
		return errors.New("lending: external repayments are not enabled")
	}
	p, ok, err := s.store.ClaimExternalPayment(ctx, paymentID, s.now(), s.cfg.ExternalPaymentLease)
	if err != nil || !ok {
		return err
	}
	for range 4 { // verify -> credit -> apply is three steps
		next, err := s.stepExternalPayment(ctx, p)
		if err != nil || next == nil {
			return err
		}
		p = *next
	}
	return nil
}

// stepExternalPayment performs one step. It returns the payment in its new
// state when a further step can be taken immediately, or nil when it must
// wait or is finished.
func (s *Service) stepExternalPayment(ctx context.Context, p domain.ExternalPayment) (*domain.ExternalPayment, error) {
	switch {
	case p.State.AwaitingProvider():
		return s.verifyExternalPayment(ctx, p)
	case p.State == domain.ExternalVerified:
		return s.creditExternalPayment(ctx, p)
	case p.State == domain.ExternalCredited:
		return s.applyExternalPayment(ctx, p)
	default:
		// Finished: take it off the work queue.
		return nil, s.parkExternalPayment(ctx, p, "")
	}
}

// moveExternalPayment transitions the payment and returns it in its new
// state, or nil if another actor changed it first (that actor owns the next
// step) or the new state needs no further driving.
func (s *Service) moveExternalPayment(ctx context.Context, p domain.ExternalPayment, to domain.ExternalPaymentState, u postgres.ExternalPaymentUpdate) (*domain.ExternalPayment, error) {
	if err := domain.ValidateTransition(p.State, to); err != nil {
		return nil, err
	}
	u.To = to
	now := s.now()
	// Detached from the caller's cancellation: once the provider has
	// answered or the ledger has posted, the record of it must be written
	// even if the request that triggered it has gone away.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	var moved bool
	err := s.store.InTx(writeCtx, func(q *postgres.Queries) error {
		var err error
		moved, err = q.TransitionExternalPayment(writeCtx, p.ID, p.State, u, now)
		if err != nil || !moved {
			return err
		}
		detail := map[string]any{"from": string(p.State), "code": u.Code, "reference": p.Reference}
		if u.VerifiedMinor != nil {
			detail["verified_minor"] = *u.VerifiedMinor
		}
		if u.JournalID != nil {
			detail["journal_id"] = *u.JournalID
		}
		if err := q.Audit(writeCtx, postgres.SystemActor, "EXTERNAL_PAYMENT_"+string(to), "external_payment", p.ID.String(), detail); err != nil {
			return err
		}
		switch to {
		case domain.ExternalCredited, domain.ExternalApplied, domain.ExternalUnapplied, domain.ExternalFailed:
			loan, err := q.GetLoan(writeCtx, p.LoanID)
			if err != nil {
				return err
			}
			ev := loanEvent{ExternalPaymentID: p.ID.String(), Detail: u.Code}
			if p.VerifiedMinor != nil {
				ev.AmountMinor = *p.VerifiedMinor
			}
			return s.emitLoan(writeCtx, q, "loan.external_payment."+lower(string(to)), loan, ev)
		}
		return nil
	})
	if err != nil || !moved {
		return nil, err
	}
	s.metrics.ExternalPaymentStateChanged(string(to))
	if u.NextAttemptAt == nil {
		return nil, nil
	}
	p.State = to
	if u.VerifiedMinor != nil {
		p.VerifiedMinor = u.VerifiedMinor
	}
	if u.JournalID != nil {
		p.JournalID = u.JournalID
	}
	return &p, nil
}

// retryExternalPayment keeps the payment in its state and schedules the
// next attempt.
func (s *Service) retryExternalPayment(ctx context.Context, p domain.ExternalPayment, steps []time.Duration, code string) error {
	now := s.now()
	next := now.Add(backoff(steps, p.Attempts+1))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.store.TransitionExternalPayment(writeCtx, p.ID, p.State,
		postgres.ExternalPaymentUpdate{To: p.State, Code: code, NextAttemptAt: &next, CountAttempt: true}, now)
	return err
}

// parkExternalPayment takes the payment off the work queue without changing
// its state.
func (s *Service) parkExternalPayment(ctx context.Context, p domain.ExternalPayment, code string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.store.TransitionExternalPayment(writeCtx, p.ID, p.State, postgres.ExternalPaymentUpdate{To: p.State, Code: code}, s.now())
	return err
}
