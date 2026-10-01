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

// intentOpType is the ledger posting-reference type for a posting intent.
func intentOpType(kind domain.IntentKind) string { return "LENDING_" + string(kind) }

// ProcessIntent posts one intent to the ledger and applies its effect to
// lending. This is the single path by which lending moves money, and it is
// built to be repeated:
//
//	step 1  (already done, in the caller's transaction) intent row committed
//	step 2  ledger.Post with the intent id as posting reference
//	step 3  one transaction: mark the intent POSTED and apply its effect
//
// A crash after step 1: the sweeper calls ProcessIntent again.
// A crash after step 2 (the ledger has posted, lending does not know): the
// sweeper calls ProcessIntent again; the ledger recognises the reference and
// returns the original journal without posting a second one; step 3 runs.
// A crash after step 3: the intent is no longer PENDING; nothing happens.
// Two workers at once: both may reach step 2 (one journal results); step 3
// locks the intent row and only the first applies.
//
// A ledger answer of "refused" (insufficient funds, inactive account) is a
// business outcome and is applied as a rejection. Anything else (timeout,
// unavailable) is unknown: the intent stays PENDING and is retried with the
// same reference, never abandoned and never re-created under a new one.
func (s *Service) ProcessIntent(ctx context.Context, intentID uuid.UUID) error {
	intent, err := s.store.GetIntent(ctx, intentID)
	if err != nil {
		return err
	}
	if intent.State != domain.IntentPending {
		return nil
	}

	postCtx, cancel := context.WithTimeout(ctx, s.cfg.LedgerTimeout)
	posted, err := s.ledger.Post(postCtx, PostRequest{
		Ref:          JournalRef{OpType: intentOpType(intent.Kind), OpID: intent.ID, OpStep: "POST"},
		JournalType:  intent.JournalType,
		Lines:        intent.Lines,
		BusinessDate: intent.BusinessDate,
	})
	cancel()

	switch {
	case err == nil:
		if posted.AlreadyPosted {
			s.metrics.DuplicatePostingPrevented(string(intent.Kind))
		}
		if err := s.applyIntent(ctx, intent.ID, &posted, ""); err != nil {
			return fmt.Errorf("apply posted intent: %w", err)
		}
		s.metrics.IntentCompleted(string(intent.Kind), "posted")
		return nil

	case errors.Is(err, ErrLedgerInsufficientFunds), errors.Is(err, ErrLedgerRejected):
		reason := "LEDGER_REJECTED"
		if errors.Is(err, ErrLedgerInsufficientFunds) {
			reason = "INSUFFICIENT_FUNDS"
		}
		if err := s.applyIntent(ctx, intent.ID, nil, reason); err != nil {
			return fmt.Errorf("apply rejected intent: %w", err)
		}
		s.metrics.IntentCompleted(string(intent.Kind), "rejected")
		return nil

	default:
		// Outcome unknown. Schedule another attempt with the same reference.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		next := s.now().Add(backoff(s.cfg.RetryBackoff, intent.Attempts+1))
		if derr := s.store.DeferIntent(writeCtx, intent.ID, next); derr != nil {
			return errors.Join(err, derr)
		}
		s.metrics.IntentCompleted(string(intent.Kind), "deferred")
		return fmt.Errorf("ledger post for intent %s: %w", intent.ID, err)
	}
}

// ProcessDueIntents drives every pending intent whose attempt is due. It is
// the recovery path for ProcessIntent.
func (s *Service) ProcessDueIntents(ctx context.Context) error {
	ids, err := s.store.DueIntents(ctx, s.now(), 100)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := s.ProcessIntent(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// applyIntent records the outcome of an intent and applies its effect, in
// one transaction. posted is nil for a rejection.
//
// Lock order is loan, then intent: the same order every other loan operation
// uses (lock the loan, then insert or touch its intent), so they queue
// instead of deadlocking.
func (s *Service) applyIntent(ctx context.Context, intentID uuid.UUID, posted *PostedJournal, rejectReason string) error {
	// after carries work that must happen once the transaction has committed
	// (metrics, and kicking the next durable step).
	var after []func()
	err := s.store.InTx(ctx, func(q *postgres.Queries) error {
		after = after[:0]
		peek, err := q.GetIntent(ctx, intentID)
		if err != nil {
			return err
		}
		var loan *domain.Loan
		if peek.LoanID != nil {
			l, err := q.LockLoan(ctx, *peek.LoanID)
			if err != nil {
				return err
			}
			loan = &l
		}
		intent, err := q.LockIntent(ctx, intentID)
		if err != nil {
			return err
		}
		if intent.State != domain.IntentPending {
			return nil // another worker applied it
		}
		now := s.now()

		if posted != nil {
			if err := q.MarkIntentPosted(ctx, intent.ID, posted.ID, now); err != nil {
				return err
			}
		} else if err := q.MarkIntentRejected(ctx, intent.ID, rejectReason, now); err != nil {
			return err
		}

		switch intent.Kind {
		case domain.IntentDisbursement:
			fn, err := s.applyDisbursement(ctx, q, *loan, posted, rejectReason, now)
			after = append(after, fn...)
			return err
		case domain.IntentRepayment:
			return s.applyRepayment(ctx, q, intent, *loan, posted, rejectReason, now)
		case domain.IntentRecovery:
			return s.applyRecovery(ctx, q, intent, *loan, posted, rejectReason, now)
		case domain.IntentLateFee:
			return s.applyLateFee(ctx, q, intent, *loan, posted, rejectReason, now)
		case domain.IntentAccrual:
			return s.applyAccrual(ctx, q, intent, posted, rejectReason, now)
		case domain.IntentWriteOff:
			return s.applyWriteOff(ctx, q, intent, *loan, posted, rejectReason, now)
		default:
			return fmt.Errorf("unknown intent kind %q", intent.Kind)
		}
	})
	if err != nil {
		return err
	}
	for _, fn := range after {
		fn()
	}
	return nil
}

func (s *Service) applyAccrual(ctx context.Context, q *postgres.Queries, intent domain.PostingIntent, posted *PostedJournal, rejectReason string, now time.Time) error {
	if posted == nil {
		_, err := q.OpenException(ctx, domain.ExceptionPostingRejected, "posting_intent", intent.ID.String(), 0,
			map[string]any{"kind": "ACCRUAL", "reason": rejectReason})
		return err
	}
	return q.MarkAccrualRunPosted(ctx, intent.ID, now)
}
