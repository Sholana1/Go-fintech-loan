package app

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// External repayments: a customer repays from outside the bank (card, or a
// transfer from another bank) through a payment provider.
//
// The rule that shapes this flow: WE NEVER BELIEVE A CLAIM THAT MONEY WAS
// PAID. Not the customer's app ("payment successful"), and not a webhook,
// even a correctly signed one. The only thing that moves money in our books
// is the provider's own answer to "what happened to reference X?", asked by
// us over an authenticated call (PaymentVerifier.VerifyPayment).
//
//	1. InitiateExternalRepayment   we choose a reference and store the
//	                               payment as INITIATED. No money moves.
//	2. the customer pays at the provider using that reference
//	3. a webhook (fast path) or the poller (safety net) triggers the driver
//	4. driver: ask the provider -> SUCCESS -> VERIFIED
//	5. driver: credit the customer's deposit account in the ledger,
//	           idempotent on the payment id            -> CREDITED
//	6. driver: take the repayment from that account, the same code path as
//	           any other repayment                     -> APPLIED
//
// Why credit the deposit account first instead of crediting the loan
// directly: money that arrived belongs to the customer whatever the loan
// needs. If they paid more than is owed, or the loan was settled in the
// meantime, the surplus is already where it belongs, in their account, and
// no refund process is needed. It also means the allocation rules exist in
// exactly one place.

const (
	endpointExternalRepayment = "POST /v1/loans/{id}/external-repayments"
	// endpointExternalApply scopes the idempotency key of the internal
	// repayment that applies an external payment. It is not reachable from
	// the API, so a customer cannot occupy the key.
	endpointExternalApply = "internal:external-payment-apply"
)

// ExternalRepaymentsEnabled reports whether a payment provider is configured.
func (s *Service) ExternalRepaymentsEnabled() bool { return s.collect != nil }

// InitiateExternalRepayment records the customer's intention to pay
// amountMinor towards a loan from outside the bank and returns the reference
// they must pay with. It moves no money.
func (s *Service) InitiateExternalRepayment(ctx context.Context, customerID, loanID uuid.UUID, idemKey string, amountMinor int64) (p domain.ExternalPayment, replay bool, err error) {
	if s.collect == nil {
		return p, false, fmt.Errorf("%w: external repayments are not available", domain.ErrConflict)
	}
	if err := validateIdemKey(idemKey); err != nil {
		return p, false, err
	}
	if amountMinor <= 0 {
		return p, false, fmt.Errorf("%w: amount must be positive", domain.ErrValidation)
	}
	hash, err := requestHash(struct {
		Loan   uuid.UUID
		Amount int64
	}{loanID, amountMinor})
	if err != nil {
		return p, false, err
	}

	now := s.now()
	id := uuid.New()
	var existing uuid.UUID
	err = s.store.InTx(ctx, func(q *postgres.Queries) error {
		prior, claimed, err := q.ClaimIdempotencyKey(ctx, customerID, endpointExternalRepayment, idemKey, hash, id, now.Add(s.cfg.IdempotencyRetention))
		if err != nil {
			return err
		}
		if !claimed {
			existing = prior
			return nil
		}
		// Someone else's loan looks exactly like a loan that does not exist.
		loan, err := q.GetLoanForCustomer(ctx, loanID, customerID)
		if err != nil {
			return err
		}
		owes := loan.State.Servicing() || (loan.State == domain.LoanWrittenOff && loan.WrittenOffMinor > loan.RecoveredMinor)
		if !owes {
			return fmt.Errorf("%w: loan is %s", domain.ErrLoanNotRepayable, loan.State)
		}
		p = domain.ExternalPayment{
			ID: id, LoanID: loan.ID, CustomerID: customerID,
			// Lower case, hyphens only: the strictest reference format among
			// the providers we have a verified contract for.
			Reference: "rp-" + id.String(), Provider: s.collect.Name(),
			ExpectedMinor: amountMinor, Currency: string(s.product.Currency),
			State: domain.ExternalInitiated, ExpiresAt: now.Add(s.cfg.ExternalPaymentValidity),
			StateChanged: now, CreatedAt: now,
		}
		if err := q.InsertExternalPayment(ctx, p, now.Add(backoff(s.cfg.ExternalPaymentPollBackoff, 1))); err != nil {
			return err
		}
		return q.Audit(ctx, postgres.Actor{Kind: "customer", ID: customerID.String()}, "EXTERNAL_PAYMENT_INITIATED", "loan", loan.ID.String(),
			map[string]any{"payment_id": id.String(), "reference": p.Reference, "expected_minor": amountMinor, "provider": p.Provider})
	})
	if err != nil {
		return domain.ExternalPayment{}, false, err
	}
	if existing != uuid.Nil {
		s.metrics.IdempotentReplay(endpointExternalRepayment)
		p, err = s.store.GetExternalPayment(ctx, existing)
		return p, true, err
	}
	s.metrics.ExternalPaymentStateChanged(string(domain.ExternalInitiated))
	return p, false, nil
}

// GetExternalRepayment returns one of the customer's external payments.
func (s *Service) GetExternalRepayment(ctx context.Context, customerID, loanID, paymentID uuid.UUID) (domain.ExternalPayment, error) {
	return s.store.GetExternalPaymentForCustomer(ctx, paymentID, loanID, customerID)
}
