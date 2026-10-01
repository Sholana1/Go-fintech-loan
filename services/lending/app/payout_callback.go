package app

import (
	"context"
	"errors"
	"fmt"

	"bankplatform.internal/services/lending/domain"
)

// PayoutCallback is a provider's notification about a transfer, after its
// signature has been verified by the transport layer.
type PayoutCallback struct {
	Provider  string
	EventID   string
	Reference string
	// Outcome is what the callback says happened. An adapter leaves it
	// empty when it does not trust the callback body for that: the service
	// then asks the provider (a status query) instead of believing it.
	Outcome     domain.ProviderOutcome
	ProviderRef string
	Code        string
	Raw         []byte
}

// HandlePayoutCallback applies a provider callback.
//
// Callbacks may arrive more than once, late, or out of order. None of that
// matters: the outcome is applied through applyProviderOutcome, which is
// idempotent and state-aware, and the event id is recorded so a duplicate is
// recognised and counted. A callback is an accelerator only; if one is lost,
// the status query and the settlement report reach the same result.
func (s *Service) HandlePayoutCallback(ctx context.Context, cb PayoutCallback) error {
	if cb.EventID == "" || cb.Reference == "" {
		return fmt.Errorf("%w: callback needs an event id and a reference", domain.ErrValidation)
	}
	p, err := s.store.PayoutByReference(ctx, cb.Reference)
	if errors.Is(err, domain.ErrNotFound) {
		// A callback for a transfer we never created. Record it and alert.
		if _, rerr := s.store.RecordPayoutEvent(ctx, cb.Provider, cb.EventID, cb.Reference, string(cb.Outcome), cb.Raw); rerr != nil {
			return rerr
		}
		s.metrics.CallbackReceived("unknown_reference")
		return s.raise(ctx, domain.ExceptionUnknownAtUs, "payout_reference", cb.Reference, 0, map[string]any{"source": "callback", "provider": cb.Provider})
	}
	if err != nil {
		return err
	}

	res := PayoutResult{Outcome: cb.Outcome, ProviderRef: cb.ProviderRef, Code: cb.Code}
	if cb.Outcome == "" {
		if s.payouts == nil {
			return errors.New("lending: payouts are not enabled")
		}
		queryCtx, cancel := context.WithTimeout(ctx, s.cfg.PayoutTimeout)
		res, err = s.payouts.Query(queryCtx, cb.Reference)
		cancel()
		if err != nil {
			// Could not ask. Fail the delivery so the provider sends it
			// again; the status-query schedule covers us if it does not.
			return fmt.Errorf("%w: payout status query: %v", domain.ErrDependencyUnavailable, err)
		}
	}

	// Apply first, then record. If the process dies between the two, the
	// provider's retry applies the same outcome again (a no-op) and records.
	if err := s.applyProviderOutcome(ctx, p, res, "callback"); err != nil {
		return err
	}
	first, err := s.store.RecordPayoutEvent(ctx, cb.Provider, cb.EventID, cb.Reference, string(res.Outcome), cb.Raw)
	if err != nil {
		return err
	}
	if first {
		s.metrics.CallbackReceived("applied")
	} else {
		s.metrics.CallbackReceived("duplicate")
	}
	return nil
}
