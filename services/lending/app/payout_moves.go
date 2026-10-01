package app

import (
	"context"
	"errors"
	"time"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// movePayout transitions the payout and returns it in its new state, or nil
// if another actor changed it first (in which case this worker stops; the
// other actor owns the next step).
func (s *Service) movePayout(ctx context.Context, p domain.Payout, to domain.PayoutState, u postgres.PayoutUpdate, continueDriving bool) (*domain.Payout, error) {
	if err := domain.ValidateTransition(p.State, to); err != nil {
		return nil, err
	}
	u.To = to
	now := s.now()
	// State changes use a context detached from the caller's cancellation:
	// once a ledger or provider call has taken effect, the record of it must
	// be written even if the request that triggered it has gone away.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	var moved bool
	err := s.store.InTx(writeCtx, func(q *postgres.Queries) error {
		var err error
		moved, err = q.TransitionPayout(writeCtx, p.ID, p.State, u, now)
		if err != nil || !moved {
			return err
		}
		loan, err := q.GetLoan(writeCtx, p.LoanID)
		if err != nil {
			return err
		}
		if err := q.Audit(writeCtx, postgres.SystemActor, "PAYOUT_"+string(to), "payout", p.ID.String(),
			map[string]any{"from": string(p.State), "code": u.Code, "provider_ref": u.ProviderRef}); err != nil {
			return err
		}
		switch to {
		case domain.PayoutSucceeded, domain.PayoutFailed, domain.PayoutSettled:
			return s.emitLoan(writeCtx, q, "loan.payout."+lower(string(to)), loan,
				loanEvent{PayoutID: p.ID.String(), AmountMinor: p.AmountMinor, Detail: u.Code})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !moved {
		return nil, nil
	}
	s.metrics.PayoutStateChanged(string(to))
	if !continueDriving {
		return nil, nil
	}
	p.State = to
	if u.ProviderRef != "" {
		p.ProviderRef = u.ProviderRef
	}
	return &p, nil
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func (s *Service) retryPayoutLater(ctx context.Context, p domain.Payout, steps []time.Duration, cause error) error {
	now := s.now()
	next := now.Add(backoff(steps, p.Attempts+1))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.store.TransitionPayout(writeCtx, p.ID, p.State, postgres.PayoutUpdate{To: p.State, NextAttemptAt: &next, CountAttempt: true}, now); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// keepUnknown leaves the payout unresolved, schedules the next status query,
// and raises an operations exception once it has been unresolved too long.
func (s *Service) keepUnknown(ctx context.Context, p domain.Payout, code string) error {
	now := s.now()
	next := now.Add(backoff(s.cfg.PayoutQueryBackoff, p.Attempts+1))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	from := p.State
	if from == domain.PayoutSending {
		if _, err := s.movePayout(ctx, p, domain.PayoutUnknown, postgres.PayoutUpdate{Code: code, NextAttemptAt: &next}, false); err != nil {
			return err
		}
	} else if _, err := s.store.TransitionPayout(writeCtx, p.ID, domain.PayoutUnknown,
		postgres.PayoutUpdate{To: domain.PayoutUnknown, Code: code, NextAttemptAt: &next, CountAttempt: true}, now); err != nil {
		return err
	}

	if now.Sub(p.StateChanged) >= s.cfg.UnresolvedAlertAfter {
		opened, err := s.store.OpenException(writeCtx, domain.ExceptionUnresolved, "payout", p.ID.String(), p.AmountMinor,
			map[string]any{"reference": p.Reference, "since": p.StateChanged.Format(time.RFC3339), "last_code": code})
		if err != nil {
			return err
		}
		if opened {
			s.metrics.ReconException(domain.ExceptionUnresolved)
		}
	}
	return nil
}
