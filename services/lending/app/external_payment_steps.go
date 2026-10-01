package app

import (
	"context"
	"errors"
	"time"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// verifyExternalPayment asks the provider what happened to the reference.
func (s *Service) verifyExternalPayment(ctx context.Context, p domain.ExternalPayment) (*domain.ExternalPayment, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, s.cfg.CollectionTimeout)
	start := time.Now()
	status, err := s.collect.VerifyPayment(verifyCtx, p.Reference)
	cancel()
	if err != nil {
		// Unknown. Not failed: the customer may well have paid. Ask again.
		s.metrics.ProviderCall(s.collect.Name(), "verify_payment", "error", time.Since(start))
		s.log.WarnContext(ctx, "payment verification outcome unknown", "payment_id", p.ID.String(), "error", err.Error())
		if !s.now().Before(p.ExpiresAt) {
			// Still no answer after the validity window. We keep asking
			// (unknown never becomes "not paid"), and tell a person.
			if rerr := s.raise(ctx, domain.ExceptionPaymentUnresolved, "external_payment", p.ID.String(), p.ExpectedMinor,
				map[string]any{"reference": p.Reference, "since": p.CreatedAt.Format(time.RFC3339)}); rerr != nil {
				return nil, rerr
			}
		}
		return nil, s.retryExternalPayment(ctx, p, s.cfg.ExternalPaymentPollBackoff, "VERIFY_UNAVAILABLE")
	}
	s.metrics.ProviderCall(s.collect.Name(), "verify_payment", string(status.Outcome), time.Since(start))

	switch status.Outcome {
	case domain.ProviderSuccess:
		if status.Currency != p.Currency || status.AmountMinor <= 0 {
			// Real money, but not something we can credit by rule.
			if _, err := s.moveExternalPayment(ctx, p, domain.ExternalReview,
				postgres.ExternalPaymentUpdate{ProviderRef: status.ProviderRef, Code: "CURRENCY_OR_AMOUNT_UNEXPECTED"}); err != nil {
				return nil, err
			}
			return nil, s.raise(ctx, domain.ExceptionPaymentCurrency, "external_payment", p.ID.String(), status.AmountMinor,
				map[string]any{"reference": p.Reference, "provider_currency": status.Currency, "expected_currency": p.Currency})
		}
		now := s.now()
		amount := status.AmountMinor
		code := orDefault(status.Code, "CONFIRMED")
		if amount != p.ExpectedMinor {
			// The customer paid a different amount from the one they
			// announced. What arrived is what is credited.
			code = "AMOUNT_DIFFERS_FROM_EXPECTED"
		}
		return s.moveExternalPayment(ctx, p, domain.ExternalVerified,
			postgres.ExternalPaymentUpdate{ProviderRef: status.ProviderRef, Code: code, VerifiedMinor: &amount, NextAttemptAt: &now})

	case domain.ProviderFailed:
		if p.State != domain.ExternalInitiated {
			return nil, s.parkExternalPayment(ctx, p, status.Code)
		}
		_, err := s.moveExternalPayment(ctx, p, domain.ExternalFailed,
			postgres.ExternalPaymentUpdate{ProviderRef: status.ProviderRef, Code: orDefault(status.Code, "PROVIDER_FAILED")})
		return nil, err

	default: // PENDING or NOT_FOUND: the customer has not (yet) paid
		if p.State != domain.ExternalInitiated {
			return nil, s.parkExternalPayment(ctx, p, "")
		}
		if !s.now().Before(p.ExpiresAt) {
			_, err := s.moveExternalPayment(ctx, p, domain.ExternalExpired, postgres.ExternalPaymentUpdate{Code: "NOT_PAID_IN_TIME"})
			return nil, err
		}
		return nil, s.retryExternalPayment(ctx, p, s.cfg.ExternalPaymentPollBackoff, "AWAITING_PAYMENT")
	}
}

// creditExternalPayment records the confirmed money in the ledger:
//
//	Dr Collections clearing (the provider owes us)   verified amount
//	Cr Customer deposit                              verified amount
func (s *Service) creditExternalPayment(ctx context.Context, p domain.ExternalPayment) (*domain.ExternalPayment, error) {
	loan, err := s.store.GetLoan(ctx, p.LoanID)
	if err != nil {
		return nil, err
	}
	amount := *p.VerifiedMinor
	postCtx, cancel := context.WithTimeout(ctx, s.cfg.LedgerTimeout)
	posted, err := s.ledger.Post(postCtx, PostRequest{
		Ref:         JournalRef{OpType: opExternalPayment, OpID: p.ID, OpStep: "CREDIT"},
		JournalType: contract.JournalExternalCollection,
		Lines: []domain.JournalLine{
			{AccountCode: contract.AccountCollectionsClearing, Direction: "DEBIT", AmountMinor: amount},
			{AccountCode: loan.DepositAccountCode, Direction: "CREDIT", AmountMinor: amount},
		},
		Narrative: "External payment " + p.Reference,
	})
	cancel()
	switch {
	case err == nil:
		if posted.AlreadyPosted {
			s.metrics.DuplicatePostingPrevented("EXTERNAL_PAYMENT")
		}
		now := s.now()
		return s.moveExternalPayment(ctx, p, domain.ExternalCredited, postgres.ExternalPaymentUpdate{JournalID: &posted.ID, NextAttemptAt: &now})
	case errors.Is(err, ErrLedgerRejected), errors.Is(err, ErrLedgerInsufficientFunds):
		// The ledger refuses to credit the customer (account closed or
		// frozen). The money is real and is not ours: a person must decide.
		if rerr := s.raise(ctx, domain.ExceptionPostingRejected, "external_payment", p.ID.String(), amount,
			map[string]any{"reference": p.Reference, "error": err.Error()}); rerr != nil {
			return nil, rerr
		}
		return nil, s.retryExternalPayment(ctx, p, s.cfg.RetryBackoff, "LEDGER_REJECTED")
	default:
		// Unknown: repeat with the same reference.
		return nil, errors.Join(err, s.retryExternalPayment(ctx, p, s.cfg.RetryBackoff, "LEDGER_UNAVAILABLE"))
	}
}

// applyExternalPayment takes the repayment from the customer's account. The
// repayment's id is the payment's id, so this step can always find out
// whether it has already been done.
func (s *Service) applyExternalPayment(ctx context.Context, p domain.ExternalPayment) (*domain.ExternalPayment, error) {
	rep, err := s.store.GetRepayment(ctx, p.ID)
	if errors.Is(err, domain.ErrNotFound) {
		rep, _, err = s.repay(ctx, repayCommand{
			CustomerID: p.CustomerID, LoanID: p.LoanID, Endpoint: endpointExternalApply, IdemKey: p.ID.String(),
			AmountMinor: *p.VerifiedMinor, Source: domain.SourceExternal, RepaymentID: p.ID,
		})
	}
	switch {
	case errors.Is(err, domain.ErrLoanNotRepayable):
		// Settled or closed in the meantime. The money is already in the
		// customer's account, which is where it should stay.
		_, err := s.moveExternalPayment(ctx, p, domain.ExternalUnapplied, postgres.ExternalPaymentUpdate{Code: "LOAN_NOT_REPAYABLE"})
		return nil, err
	case errors.Is(err, domain.ErrOperationInProgress):
		// Another posting on the loan is in flight; it will be done shortly.
		return nil, s.retryExternalPayment(ctx, p, s.cfg.RetryBackoff, "LOAN_BUSY")
	case err != nil:
		return nil, errors.Join(err, s.retryExternalPayment(ctx, p, s.cfg.RetryBackoff, "APPLY_FAILED"))
	}

	switch rep.State {
	case domain.RepaymentAllocated:
		_, err := s.moveExternalPayment(ctx, p, domain.ExternalApplied, postgres.ExternalPaymentUpdate{RepaymentID: &rep.ID, Code: "APPLIED"})
		return nil, err
	case domain.RepaymentRejected:
		_, err := s.moveExternalPayment(ctx, p, domain.ExternalUnapplied,
			postgres.ExternalPaymentUpdate{RepaymentID: &rep.ID, Code: orDefault(rep.RejectReason, "REPAYMENT_REJECTED")})
		return nil, err
	default:
		// The repayment's posting is durable and the posting sweeper will
		// complete it; look again shortly.
		return nil, s.retryExternalPayment(ctx, p, s.cfg.RetryBackoff, "REPAYMENT_PENDING")
	}
}
