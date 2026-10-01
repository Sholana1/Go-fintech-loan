package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/lending/domain"
)

// CollectDue attempts the scheduled debits that are due.
//
// A collection is a repayment from the customer's own deposit account that
// the customer authorised when accepting the offer. It debits at most what
// is due (never pays ahead), at most CollectionAttemptsPerDay times a day,
// and only through the same Repay path as a customer-initiated repayment, so
// the ledger still decides whether the funds are there.
func (s *Service) CollectDue(ctx context.Context) error {
	now, today := s.now(), s.today()
	ids, err := s.store.LoansDueForCollection(ctx, now, 100)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := s.collectLoan(ctx, id, now, today); err != nil {
			errs = append(errs, fmt.Errorf("loan %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) collectLoan(ctx context.Context, loanID uuid.UUID, now, today time.Time) error {
	retryAt := now.Add(time.Duration(s.product.CollectionRetryAfterHours) * time.Hour)
	attempt, ok, err := s.store.ClaimCollection(ctx, loanID, now, today, retryAt)
	if err != nil || !ok {
		return err
	}
	loan, err := s.store.GetLoan(ctx, loanID)
	if err != nil {
		return err
	}
	insts, err := s.store.Instalments(ctx, loan.ID, loan.ScheduleVersion)
	if err != nil {
		return err
	}
	due := domain.AmountDue(insts, today)
	if due == 0 {
		return s.store.ScheduleCollection(ctx, loan.ID, nextDueStart(insts, today))
	}
	if attempt > s.product.CollectionAttemptsPerDay {
		// Enough for today. Try again tomorrow morning.
		tomorrow := bizdate.StartOf(today.AddDate(0, 0, 1)).Add(6 * time.Hour)
		return s.store.ScheduleCollection(ctx, loan.ID, &tomorrow)
	}

	balanceCtx, cancel := context.WithTimeout(ctx, s.cfg.LedgerTimeout)
	available, err := s.ledger.Available(balanceCtx, loan.DepositAccountCode)
	cancel()
	if err != nil {
		return err // the lease (retryAt) schedules the retry
	}
	amount := min(available, due)
	if amount <= 0 || (!s.product.AllowPartialCollection && available < due) {
		return nil
	}

	// A deterministic key: if this attempt is repeated after a crash it is
	// the same repayment, not a second debit.
	key := fmt.Sprintf("auto:%s:%s:%d", loan.ID, today.Format(time.DateOnly), attempt)
	rep, _, err := s.Repay(ctx, loan.CustomerID, loan.ID, key, amount, domain.SourceAutoDebit)
	if errors.Is(err, domain.ErrOperationInProgress) || errors.Is(err, domain.ErrLoanNotRepayable) {
		return nil
	}
	if err != nil {
		return err
	}
	if rep.State == domain.RepaymentAllocated && rep.AppliedMinor >= due {
		insts, err := s.store.Instalments(ctx, loan.ID, loan.ScheduleVersion)
		if err != nil {
			return err
		}
		return s.store.ScheduleCollection(ctx, loan.ID, nextDueStart(insts, today))
	}
	return nil
}

// nextDueStart returns the morning of the next instalment that still has
// something to pay and falls due after today, or nil if there is none.
func nextDueStart(insts []domain.Instalment, today time.Time) *time.Time {
	for _, i := range insts {
		if i.DueDate.After(today) && !i.FullyPaid() {
			t := bizdate.StartOf(i.DueDate)
			return &t
		}
	}
	return nil
}
