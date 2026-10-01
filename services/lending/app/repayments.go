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

const endpointRepay = "POST /v1/loans/{id}/repayments"

// Repay takes a repayment from the customer's own deposit account and
// allocates it to the loan.
//
// The amount offered is an upper bound. Only what the product rules can
// apply is debited (see domain.Allocate); the rest stays in the customer's
// account and is reported as unapplied. An overpayment therefore never
// leaves the customer's account, and an amount that covers the payoff
// figure settles the loan early.
//
// The returned repayment is ALLOCATED when the debit posted, REJECTED when
// the ledger refused it (for example insufficient funds), or PENDING when
// the ledger could not be reached: in that case the posting is durable and
// will complete, and a retry with the same idempotency key returns its
// current state.
func (s *Service) Repay(ctx context.Context, customerID, loanID uuid.UUID, idemKey string, amountMinor int64, source string) (rep domain.Repayment, replay bool, err error) {
	if err := validateIdemKey(idemKey); err != nil {
		return rep, false, err
	}
	return s.repay(ctx, repayCommand{
		CustomerID: customerID, LoanID: loanID, Endpoint: endpointRepay, IdemKey: idemKey,
		AmountMinor: amountMinor, Source: source, RepaymentID: uuid.New(),
	})
}

// repayCommand is one repayment to take from the customer's deposit account.
type repayCommand struct {
	CustomerID  uuid.UUID
	LoanID      uuid.UUID
	Endpoint    string // idempotency scope
	IdemKey     string
	AmountMinor int64
	Source      string
	// RepaymentID is chosen by the caller so that an internal caller can
	// derive it from what it is applying and find the repayment again.
	RepaymentID uuid.UUID
}

func (s *Service) repay(ctx context.Context, cmd repayCommand) (rep domain.Repayment, replay bool, err error) {
	customerID, loanID, idemKey, amountMinor, source := cmd.CustomerID, cmd.LoanID, cmd.IdemKey, cmd.AmountMinor, cmd.Source
	if amountMinor <= 0 {
		return rep, false, fmt.Errorf("%w: amount must be positive", domain.ErrValidation)
	}
	hash, err := requestHash(struct {
		Loan   uuid.UUID
		Amount int64
	}{loanID, amountMinor})
	if err != nil {
		return rep, false, err
	}

	now, today := s.now(), s.today()
	repaymentID, intentID := cmd.RepaymentID, uuid.New()
	var existing uuid.UUID

	err = s.store.InTx(ctx, func(q *postgres.Queries) error {
		id, claimed, err := q.ClaimIdempotencyKey(ctx, customerID, cmd.Endpoint, idemKey, hash, repaymentID, now.Add(s.cfg.IdempotencyRetention))
		if err != nil {
			return err
		}
		if !claimed {
			existing = id
			return nil
		}
		// Lock the loan: the allocation below is computed against the
		// schedule as it stands, and nothing else may change the schedule
		// until this repayment has been applied (the pending intent enforces
		// that after this transaction commits).
		loan, err := q.LockLoan(ctx, loanID)
		if err != nil {
			return err
		}
		if loan.CustomerID != customerID {
			// Someone else's loan looks exactly like a loan that does not exist.
			return fmt.Errorf("%w: loan", domain.ErrNotFound)
		}

		rep = domain.Repayment{
			ID: repaymentID, LoanID: loan.ID, CustomerID: customerID, Source: source,
			RequestedMinor: amountMinor, State: domain.RepaymentPending, BusinessDate: today, IntentID: intentID, CreatedAt: now,
		}
		intent := domain.PostingIntent{ID: intentID, LoanID: &loan.ID}

		switch {
		case loan.State.Servicing():
			insts, err := q.Instalments(ctx, loan.ID, loan.ScheduleVersion)
			if err != nil {
				return err
			}
			alloc, err := domain.Allocate(insts, amountMinor, today)
			if errors.Is(err, domain.ErrNothingToPay) {
				return fmt.Errorf("%w: nothing is payable on this loan today", domain.ErrLoanNotRepayable)
			}
			if err != nil {
				return err
			}
			rep.Allocation, rep.AppliedMinor, rep.UnappliedMinor = alloc, alloc.Applied, alloc.Unapplied
			intent.Kind, intent.JournalType = domain.IntentRepayment, contract.JournalLoanRepayment
			intent.Lines = repaymentLines(loan.DepositAccountCode, alloc)

		case loan.State == domain.LoanWrittenOff:
			// Money received after write-off is a recovery, not a repayment
			// of the (already derecognised) receivable.
			remaining := loan.WrittenOffMinor - loan.RecoveredMinor
			if remaining <= 0 {
				return fmt.Errorf("%w: nothing remains to recover", domain.ErrLoanNotRepayable)
			}
			applied := min(amountMinor, remaining)
			rep.IsRecovery, rep.AppliedMinor, rep.UnappliedMinor = true, applied, amountMinor-applied
			rep.Allocation = domain.Allocation{Applied: applied, Unapplied: amountMinor - applied}
			intent.Kind, intent.JournalType = domain.IntentRecovery, contract.JournalLoanRecovery
			intent.Lines = []domain.JournalLine{
				{AccountCode: loan.DepositAccountCode, Direction: "DEBIT", AmountMinor: applied},
				{AccountCode: contract.AccountRecoveriesIncome, Direction: "CREDIT", AmountMinor: applied},
			}

		default:
			return fmt.Errorf("%w: loan is %s", domain.ErrLoanNotRepayable, loan.State)
		}

		if err := q.InsertIntent(ctx, intent, now.Add(s.cfg.InlineGrace)); err != nil {
			return err // ErrOperationInProgress if another posting is in flight
		}
		return q.InsertRepayment(ctx, rep)
	})
	if err != nil {
		return domain.Repayment{}, false, err
	}
	if existing != uuid.Nil {
		s.metrics.IdempotentReplay(cmd.Endpoint)
		rep, err = s.store.GetRepayment(ctx, existing)
		return rep, true, err
	}

	if err := s.ProcessIntent(ctx, intentID); err != nil {
		s.log.WarnContext(ctx, "repayment posting did not complete inline; the sweeper will retry", "repayment_id", repaymentID.String(), "error", err.Error())
	}
	rep, err = s.store.GetRepayment(ctx, repaymentID)
	return rep, false, err
}

// repaymentLines is the accounting of a repayment (plan 6.11, example 4):
//
//	Dr Customer deposit              applied
//	Cr Loan fees receivable          fees
//	Cr Loan interest receivable      interest
//	Cr Loans receivable - principal  principal
func repaymentLines(depositAccount string, a domain.Allocation) []domain.JournalLine {
	lines := []domain.JournalLine{{AccountCode: depositAccount, Direction: "DEBIT", AmountMinor: a.Applied}}
	add := func(code string, amount int64) {
		if amount > 0 {
			lines = append(lines, domain.JournalLine{AccountCode: code, Direction: "CREDIT", AmountMinor: amount})
		}
	}
	add(contract.AccountLoansFeesReceivable, a.Fees)
	add(contract.AccountLoansInterestReceivable, a.Interest)
	add(contract.AccountLoansPrincipal, a.Principal)
	return lines
}

// applyRepayment applies a posted repayment to the schedule, or records its
// rejection. It runs under the loan's row lock.
func (s *Service) applyRepayment(ctx context.Context, q *postgres.Queries, intent domain.PostingIntent, loan domain.Loan, posted *PostedJournal, rejectReason string, now time.Time) error {
	rep, err := q.RepaymentByIntent(ctx, intent.ID)
	if err != nil {
		return err
	}
	if posted == nil {
		// The ledger refused the debit. Nothing moved; nothing is applied.
		return q.CompleteRepayment(ctx, rep.ID, domain.RepaymentRejected, nil, rejectReason, now)
	}

	insts, err := q.Instalments(ctx, loan.ID, loan.ScheduleVersion)
	if err != nil {
		return err
	}
	// Apply as of the business date the allocation was computed on, so the
	// result matches the journal that was posted even if midnight has passed.
	updated, err := domain.Apply(insts, rep.Allocation, rep.BusinessDate)
	if err != nil {
		return fmt.Errorf("apply repayment %s: %w", rep.ID, err)
	}

	update := postgres.LoanUpdate{}
	if rep.Allocation.EarlySettlement {
		// Unearned interest was waived: that changes the schedule, so it is
		// written as a new version and the old one is kept for the record.
		version := loan.ScheduleVersion + 1
		if err := q.InsertInstalments(ctx, loan.ID, version, updated); err != nil {
			return err
		}
		update.ScheduleVersion = &version
	} else if err := q.SaveInstalments(ctx, loan.ID, loan.ScheduleVersion, updated); err != nil {
		return err
	}

	eventType := "loan.repayment.allocated"
	if domain.AllPaid(updated) {
		closed := domain.LoanClosed
		update.State, update.ClosedAt = &closed, &now
		zero, current := 0, s.product.Bucket(0)
		update.DaysPastDue, update.ArrearsBucket = &zero, &current
		var none *time.Time
		update.NextCollectionAt = &none
		// If interest that was paid has not yet been recognised by the
		// accrual job, flag the loan so the job catches it up.
		lag := false
		for _, i := range updated {
			if i.InterestAccrued < i.InterestPaid {
				lag = true
			}
		}
		update.AccrualCatchup = &lag
		loan.State = closed
		eventType = "loan.closed"
	} else {
		today := s.today()
		dpd := domain.DaysPastDue(updated, today)
		bucket := s.product.Bucket(dpd)
		state := domain.LoanActive
		if dpd > 0 {
			state = domain.LoanInArrears
		}
		update.State, update.DaysPastDue, update.ArrearsBucket = &state, &dpd, &bucket
		loan.State = state
	}
	if err := q.UpdateLoan(ctx, loan.ID, update); err != nil {
		return err
	}
	if err := q.CompleteRepayment(ctx, rep.ID, domain.RepaymentAllocated, &posted.ID, "", now); err != nil {
		return err
	}
	if err := q.Audit(ctx, postgres.Actor{Kind: "customer", ID: rep.CustomerID.String()}, "REPAYMENT_ALLOCATED", "loan", loan.ID.String(),
		map[string]any{"repayment_id": rep.ID.String(), "source": rep.Source, "applied_minor": rep.AppliedMinor,
			"fees": rep.Allocation.Fees, "interest": rep.Allocation.Interest, "principal": rep.Allocation.Principal,
			"journal_id": posted.ID, "settles": rep.Allocation.Settles}); err != nil {
		return err
	}
	return s.emitLoan(ctx, q, eventType, loan, loanEvent{AmountMinor: rep.AppliedMinor, JournalID: posted.ID, RepaymentID: rep.ID.String()})
}
