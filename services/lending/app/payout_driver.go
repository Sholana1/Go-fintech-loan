package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// External payouts.
//
// A payout sends loan proceeds, already credited to the customer's deposit
// account, on to the customer's own account at another bank. It follows the
// plan's rule for every external payment (section 7.5): hold, send, resolve.
//
//	READY    place a hold on the customer's funds                 -> HELD
//	HELD     record that we are about to send (SENDING), then send
//	SENDING  provider says SUCCESS -> capture the hold            -> SUCCEEDED
//	         provider says FAILED  -> release the hold            -> FAILED
//	         anything else (timeout, error, malformed, PENDING)   -> UNKNOWN
//	UNKNOWN  query the provider with backoff until it says SUCCESS or FAILED;
//	         the settlement report is the final arbiter
//
// Rules that are never broken:
//   - A timeout or error is an unknown outcome, never a failure. The hold
//     stays in place; the money is neither returned nor spent.
//   - The transfer is sent once. From SENDING or UNKNOWN the only provider
//     call is a status query on the original reference. There is no resend
//     with a new reference and no second provider.
//   - A successful transfer cannot be undone by changing our records. If the
//     provider proves success after we recorded failure, we debit the
//     customer (late capture) or raise an exception; we do not pretend it
//     did not happen.

// releasePendingPayout makes the loan's payout ready to send once the loan
// is booked. It returns the payout id, or uuid.Nil if the loan has none.
func (s *Service) releasePendingPayout(ctx context.Context, q *postgres.Queries, loanID uuid.UUID, now time.Time) (uuid.UUID, error) {
	p, err := q.PayoutByLoan(ctx, loanID)
	if errors.Is(err, domain.ErrNotFound) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := q.TransitionPayout(ctx, p.ID, domain.PayoutPending, postgres.PayoutUpdate{To: domain.PayoutReady, NextAttemptAt: &now}, now); err != nil {
		return uuid.Nil, err
	}
	return p.ID, nil
}

// cancelPendingPayout cancels a payout whose loan was never booked.
func (s *Service) cancelPendingPayout(ctx context.Context, q *postgres.Queries, loanID uuid.UUID, now time.Time) error {
	p, err := q.PayoutByLoan(ctx, loanID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = q.TransitionPayout(ctx, p.ID, domain.PayoutPending, postgres.PayoutUpdate{To: domain.PayoutCancelled, Code: "LOAN_NOT_BOOKED"}, now)
	return err
}

// DriveDuePayouts advances every payout whose next attempt is due.
func (s *Service) DriveDuePayouts(ctx context.Context) error {
	if s.payouts == nil {
		return nil
	}
	ids, err := s.store.DuePayouts(ctx, s.now(), 50)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := s.DrivePayout(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("payout %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// DrivePayout advances one payout by as many steps as it can. The lease
// taken here ensures one worker drives a payout at a time; every state
// change is additionally a compare-and-set, so a callback or the reconciler
// acting at the same moment cannot be overwritten.
func (s *Service) DrivePayout(ctx context.Context, payoutID uuid.UUID) error {
	if s.payouts == nil {
		return errors.New("lending: payouts are not enabled")
	}
	p, ok, err := s.store.ClaimPayout(ctx, payoutID, s.now(), s.cfg.PayoutLease)
	if err != nil || !ok {
		return err
	}
	for range 4 { // READY -> HELD -> SENDING -> resolved is at most three steps
		next, err := s.stepPayout(ctx, p)
		if err != nil || next == nil {
			return err
		}
		p = *next
	}
	return nil
}

// stepPayout performs one step. It returns the payout in its new state when
// a further step can be taken immediately, or nil when the payout must wait.
func (s *Service) stepPayout(ctx context.Context, p domain.Payout) (*domain.Payout, error) {
	now := s.now()
	loan, err := s.store.GetLoan(ctx, p.LoanID)
	if err != nil {
		return nil, err
	}

	switch p.State {
	case domain.PayoutReady:
		holdCtx, cancel := context.WithTimeout(ctx, s.cfg.LedgerTimeout)
		err := s.ledger.PlaceHold(holdCtx, contract.HoldLoanPayout, p.ID, loan.DepositAccountCode, p.AmountMinor)
		cancel()
		switch {
		case err == nil:
			return s.movePayout(ctx, p, domain.PayoutHeld, postgres.PayoutUpdate{NextAttemptAt: &now}, true)
		case errors.Is(err, ErrLedgerInsufficientFunds), errors.Is(err, ErrLedgerRejected):
			// The funds are no longer there to send (or the account cannot
			// be debited). Nothing was held and nothing was sent: this is a
			// clean failure and the proceeds stay wherever they are.
			code := "HOLD_REFUSED"
			if errors.Is(err, ErrLedgerInsufficientFunds) {
				code = "INSUFFICIENT_FUNDS"
			}
			_, err := s.movePayout(ctx, p, domain.PayoutFailed, postgres.PayoutUpdate{Code: code}, false)
			return nil, err
		default:
			return nil, s.retryPayoutLater(ctx, p, s.cfg.RetryBackoff, err)
		}

	case domain.PayoutHeld:
		// Record the intent to send BEFORE calling the provider. If the
		// process dies during the call, the payout is found in SENDING and
		// is resolved by status query; it is never sent a second time.
		sendDeadline := now.Add(s.cfg.PayoutLease)
		moved, err := s.movePayout(ctx, p, domain.PayoutSending, postgres.PayoutUpdate{NextAttemptAt: &sendDeadline, CountAttempt: true}, true)
		if err != nil || moved == nil {
			return nil, err
		}
		p = *moved

		sendCtx, cancel := context.WithTimeout(ctx, s.cfg.PayoutTimeout)
		start := time.Now()
		res, err := s.payouts.Send(sendCtx, PayoutInstruction{
			Reference: p.Reference, AmountMinor: p.AmountMinor, BankCode: p.BankCode,
			AccountNumber: p.AccountNumber, AccountName: p.AccountName, Narration: "Loan disbursement",
		})
		cancel()
		if err != nil {
			s.metrics.ProviderCall(s.payouts.Name(), "send", "unknown", time.Since(start))
			s.log.WarnContext(ctx, "payout send outcome unknown", "payout_id", p.ID.String(), "error", err.Error())
			res = PayoutResult{Outcome: domain.ProviderPending, Code: "SEND_OUTCOME_UNKNOWN"}
		} else {
			s.metrics.ProviderCall(s.payouts.Name(), "send", string(res.Outcome), time.Since(start))
		}
		return nil, s.applyProviderOutcome(ctx, p, res, "send")

	case domain.PayoutSending:
		// Found in SENDING after its lease expired: the sender crashed or the
		// call outlived its deadline. The request may or may not have
		// reached the provider. Treat as unknown and query.
		moved, err := s.movePayout(ctx, p, domain.PayoutUnknown, postgres.PayoutUpdate{Code: "SENDER_INTERRUPTED", NextAttemptAt: &now}, true)
		if err != nil || moved == nil {
			return nil, err
		}
		return moved, nil

	case domain.PayoutUnknown:
		queryCtx, cancel := context.WithTimeout(ctx, s.cfg.PayoutTimeout)
		start := time.Now()
		res, err := s.payouts.Query(queryCtx, p.Reference)
		cancel()
		if err != nil {
			s.metrics.ProviderCall(s.payouts.Name(), "query", "error", time.Since(start))
			return nil, s.keepUnknown(ctx, p, "QUERY_FAILED")
		}
		s.metrics.ProviderCall(s.payouts.Name(), "query", string(res.Outcome), time.Since(start))
		return nil, s.applyProviderOutcome(ctx, p, res, "query")

	default:
		// Terminal, or waiting for the loan: nothing to drive.
		_, err := s.store.TransitionPayout(ctx, p.ID, p.State, postgres.PayoutUpdate{To: p.State}, now)
		return nil, err
	}
}
