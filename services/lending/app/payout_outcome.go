package app

import (
	"context"
	"errors"
	"time"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// applyProviderOutcome reconciles what a provider says about a transfer with
// what we have recorded. It is the one place that decision is made, used for
// the send response, status queries, callbacks and settlement reports alike,
// so they cannot disagree about what a given answer means.
//
// It is idempotent: applying the same outcome twice changes nothing the
// second time, which is what makes duplicate callbacks harmless.
func (s *Service) applyProviderOutcome(ctx context.Context, p domain.Payout, res PayoutResult, source string) error {
	loan, err := s.store.GetLoan(ctx, p.LoanID)
	if err != nil {
		return err
	}

	switch p.State {
	case domain.PayoutSending, domain.PayoutUnknown:
		switch res.Outcome {
		case domain.ProviderSuccess:
			return s.capturePayout(ctx, p, loan, res)
		case domain.ProviderFailed:
			return s.releasePayout(ctx, p, res)
		default: // PENDING, NOT_FOUND, anything unrecognised: still unknown
			return s.keepUnknown(ctx, p, orDefault(res.Code, "PROVIDER_"+string(res.Outcome)))
		}

	case domain.PayoutSucceeded, domain.PayoutSettled:
		if res.Outcome == domain.ProviderFailed {
			// We captured on evidence of success and the provider now says
			// failed. The customer has been debited; whether the money left
			// must be established by a person. Nothing is reversed here.
			return s.raise(ctx, domain.ExceptionProviderConflict, "payout", p.ID.String(), p.AmountMinor,
				map[string]any{"reference": p.Reference, "source": source, "provider_code": res.Code})
		}
		return nil // SUCCESS again (duplicate) or still pending elsewhere: nothing to do

	case domain.PayoutFailed:
		if res.Outcome == domain.ProviderSuccess {
			return s.resolveLateSuccess(ctx, p, loan, res, source)
		}
		return nil

	default:
		// A provider answer for a payout we have not sent (PENDING, READY,
		// HELD, CANCELLED) should be impossible.
		if res.Outcome == domain.ProviderSuccess || res.Outcome == domain.ProviderFailed {
			return s.raise(ctx, domain.ExceptionProviderConflict, "payout", p.ID.String(), p.AmountMinor,
				map[string]any{"reference": p.Reference, "source": source, "our_state": string(p.State), "provider_outcome": string(res.Outcome)})
		}
		return nil
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func captureRequest(p domain.Payout, loan domain.Loan, journalType, step string) PostRequest {
	return PostRequest{
		Ref:         JournalRef{OpType: contract.HoldLoanPayout, OpID: p.ID, OpStep: step},
		JournalType: journalType,
		Lines: []domain.JournalLine{
			{AccountCode: loan.DepositAccountCode, Direction: "DEBIT", AmountMinor: p.AmountMinor},
			{AccountCode: contract.AccountPayoutClearing, Direction: "CREDIT", AmountMinor: p.AmountMinor},
		},
	}
}

// capturePayout debits the customer by capturing the hold, then records
// success. Capture first, record second: if the process dies in between, the
// payout is still SENDING or UNKNOWN, the next query says SUCCESS again, and
// the capture is repeated harmlessly (same posting reference).
func (s *Service) capturePayout(ctx context.Context, p domain.Payout, loan domain.Loan, res PayoutResult) error {
	ledgerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.LedgerTimeout)
	posted, err := s.ledger.CaptureHold(ledgerCtx, contract.HoldLoanPayout, p.ID, captureRequest(p, loan, contract.JournalPayoutCapture, "CAPTURE"))
	cancel()
	switch {
	case err == nil:
		if posted.AlreadyPosted {
			s.metrics.DuplicatePostingPrevented("PAYOUT_CAPTURE")
		}
		_, err := s.movePayout(ctx, p, domain.PayoutSucceeded, postgres.PayoutUpdate{ProviderRef: res.ProviderRef, Code: orDefault(res.Code, "SUCCESS")}, false)
		return err
	case errors.Is(err, ErrLedgerHoldNotActive), errors.Is(err, ErrLedgerHoldNotFound), errors.Is(err, ErrLedgerRejected), errors.Is(err, ErrLedgerInsufficientFunds):
		// The provider paid but we cannot debit through the hold. This must
		// not be swallowed: the bank has paid out money it has not taken.
		// Raise it, and keep the payout unresolved (with backoff) so it
		// stays visible instead of being recorded as either outcome.
		if rerr := s.raise(ctx, domain.ExceptionLateSuccess, "payout", p.ID.String(), p.AmountMinor,
			map[string]any{"reference": p.Reference, "problem": "capture refused", "error": err.Error()}); rerr != nil {
			return rerr
		}
		return s.keepUnknown(ctx, p, "CAPTURE_REFUSED")
	default:
		// Ledger unreachable: stay unresolved and try again.
		return s.keepUnknown(ctx, p, "CAPTURE_PENDING")
	}
}

// releasePayout returns the held funds to the customer's available balance
// after a definite failure, then records the failure.
func (s *Service) releasePayout(ctx context.Context, p domain.Payout, res PayoutResult) error {
	ledgerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.LedgerTimeout)
	err := s.ledger.ReleaseHold(ledgerCtx, contract.HoldLoanPayout, p.ID)
	cancel()
	switch {
	case err == nil, errors.Is(err, ErrLedgerHoldNotFound):
		_, err := s.movePayout(ctx, p, domain.PayoutFailed, postgres.PayoutUpdate{ProviderRef: res.ProviderRef, Code: orDefault(res.Code, "FAILED")}, false)
		return err
	case errors.Is(err, ErrLedgerHoldCaptured):
		// We were told it failed, but the hold has already been captured as a
		// success. The two answers conflict; a person decides.
		return s.raise(ctx, domain.ExceptionProviderConflict, "payout", p.ID.String(), p.AmountMinor,
			map[string]any{"reference": p.Reference, "problem": "failure reported after capture"})
	default:
		return s.keepUnknown(ctx, p, "RELEASE_PENDING")
	}
}

// resolveLateSuccess handles proof of success arriving after we recorded a
// failure and released the customer's funds. The transfer happened, so the
// customer is debited now. If their balance no longer covers it, the debit
// is refused by the ledger and the exception stays open as a receivable for
// operations to recover; the payout is not marked successful until the
// books agree.
func (s *Service) resolveLateSuccess(ctx context.Context, p domain.Payout, loan domain.Loan, res PayoutResult, source string) error {
	if err := s.raise(ctx, domain.ExceptionLateSuccess, "payout", p.ID.String(), p.AmountMinor,
		map[string]any{"reference": p.Reference, "source": source, "provider_ref": res.ProviderRef}); err != nil {
		return err
	}
	ledgerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.LedgerTimeout)
	req := captureRequest(p, loan, contract.JournalPayoutLateCapture, "LATE_CAPTURE")
	_, err := s.ledger.Post(ledgerCtx, req)
	cancel()
	if err != nil {
		// Insufficient funds or ledger unavailable: leave the exception open.
		s.log.ErrorContext(ctx, "late payout success could not be debited", "payout_id", p.ID.String(), "error", err.Error())
		return nil
	}
	if _, err := s.movePayout(ctx, p, domain.PayoutSucceeded, postgres.PayoutUpdate{ProviderRef: res.ProviderRef, Code: "LATE_SUCCESS"}, false); err != nil {
		return err
	}
	return s.store.ResolveExceptionsFor(ctx, domain.ExceptionLateSuccess, "payout", p.ID.String(),
		"late success debited from the customer's account by late-capture journal", s.now())
}

func (s *Service) raise(ctx context.Context, kind, entityType, entityID string, amount int64, detail map[string]any) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	opened, err := s.store.OpenException(writeCtx, kind, entityType, entityID, amount, detail)
	if err != nil {
		return err
	}
	if opened {
		s.metrics.ReconException(kind)
		s.log.ErrorContext(ctx, "reconciliation exception opened", "kind", kind, "entity_type", entityType, "entity_id", entityID)
	}
	return nil
}
