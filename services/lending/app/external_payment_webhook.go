package app

import (
	"context"
	"errors"
	"fmt"

	"bankplatform.internal/services/lending/domain"
)

// PaymentNotice is a provider's webhook about an inbound payment, after its
// signature has been verified by the transport layer.
//
// It deliberately has no amount and no status. Whatever the webhook body
// says about those is ignored: a webhook only tells us WHEN to ask the
// provider, never WHAT happened.
type PaymentNotice struct {
	Provider  string
	EventID   string
	Reference string
	Raw       []byte
}

// HandlePaymentNotice reacts to a provider webhook by verifying the payment
// with the provider now instead of waiting for the next poll.
//
// Duplicates, late deliveries and out-of-order deliveries are harmless:
// the notice only wakes the payment's driver, and the driver's steps are
// compare-and-set and idempotent. If every webhook were lost the poller
// would reach the same result, later.
//
// It returns an error (so the provider retries) only when the notice could
// not be recorded. A failure while driving the payment is not the
// provider's problem: the payment stays on the work queue.
func (s *Service) HandlePaymentNotice(ctx context.Context, n PaymentNotice) error {
	if s.collect == nil {
		return errors.New("lending: external repayments are not enabled")
	}
	if n.EventID == "" || n.Reference == "" {
		return fmt.Errorf("%w: webhook needs an event id and a reference", domain.ErrValidation)
	}
	first, err := s.store.RecordExternalPaymentEvent(ctx, n.Provider, n.EventID, n.Reference, n.Raw)
	if err != nil {
		return err
	}
	p, err := s.store.ExternalPaymentByReference(ctx, n.Reference)
	if errors.Is(err, domain.ErrNotFound) {
		// A webhook for a payment we never issued a reference for: money
		// may have arrived that we cannot attribute. A person must look.
		s.metrics.CallbackReceived("payment_unknown_reference")
		return s.raise(ctx, domain.ExceptionUnknownPayment, "payment_reference", n.Reference, 0, map[string]any{"provider": n.Provider})
	}
	if err != nil {
		return err
	}
	if first {
		s.metrics.CallbackReceived("payment_notice")
	} else {
		s.metrics.CallbackReceived("payment_duplicate")
	}

	if err := s.store.WakeExternalPayment(ctx, p.ID, s.now()); err != nil {
		return err
	}
	if err := s.DriveExternalPayment(ctx, p.ID); err != nil {
		s.log.WarnContext(ctx, "external payment not completed inline; the poller will retry", "payment_id", p.ID.String(), "error", err.Error())
	}
	return nil
}
