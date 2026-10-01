package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

func accrualLockKey(d time.Time) int64 { return accrualLockBase + d.Unix()/86400 }

// AccrueInterest recognises the interest earned through the end of business
// date d: per loan in the lending subledger, and as one aggregate journal in
// the ledger (Dr interest receivable, Cr interest income).
//
// Duplicate-run safety, three independent layers:
//   - interest_accruals has primary key (loan_id, business_date);
//   - amounts are computed from cumulative targets, so a repeat finds
//     nothing left to recognise;
//   - accrual_runs has primary key (business_date) and the aggregate journal
//     is posted through an intent whose id is its ledger posting reference.
//
// The per-loan step takes a shared advisory lock for the date and the
// totalling step takes it exclusively, so the total can only be computed
// when no per-loan step is in flight, and no per-loan step can add a row for
// the date after its total has been fixed.
func (s *Service) AccrueInterest(ctx context.Context, d time.Time) error {
	if !d.Before(s.today()) {
		return fmt.Errorf("%w: interest is accrued only for completed business dates", domain.ErrValidation)
	}
	key := accrualLockKey(d)

	after := uuid.Nil
	for {
		ids, err := s.store.AccruableLoanIDs(ctx, after, 500)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.accrueLoan(ctx, id, d, key); err != nil {
				return fmt.Errorf("loan %s: %w", id, err)
			}
		}
		after = ids[len(ids)-1]
	}

	var intentID uuid.UUID
	now := s.now()
	err := s.store.InTx(ctx, func(q *postgres.Queries) error {
		intentID = uuid.Nil
		if err := q.AdvisoryLock(ctx, key, false); err != nil {
			return err
		}
		if _, exists, err := q.GetAccrualRun(ctx, d); err != nil || exists {
			return err
		}
		total, loans, err := q.AccrualTotal(ctx, d)
		if err != nil {
			return err
		}
		run := postgres.AccrualRun{BusinessDate: d, State: "EMPTY", TotalMinor: total, Loans: loans}
		if total > 0 {
			id := uuid.New()
			date := d
			if err := q.InsertIntent(ctx, domain.PostingIntent{
				ID: id, Kind: domain.IntentAccrual, JournalType: contract.JournalLoanAccrual, BusinessDate: &date,
				Lines: []domain.JournalLine{
					{AccountCode: contract.AccountLoansInterestReceivable, Direction: "DEBIT", AmountMinor: total},
					{AccountCode: contract.AccountInterestIncome, Direction: "CREDIT", AmountMinor: total},
				},
			}, now); err != nil {
				return err
			}
			run.State, run.IntentID = "CALCULATED", &id
			intentID = id
		}
		return q.InsertAccrualRun(ctx, run)
	})
	if err != nil {
		return err
	}
	if intentID != uuid.Nil {
		return s.ProcessIntent(ctx, intentID)
	}
	return nil
}

func (s *Service) accrueLoan(ctx context.Context, loanID uuid.UUID, d time.Time, lockKey int64) error {
	return s.store.InTx(ctx, func(q *postgres.Queries) error {
		if err := q.AdvisoryLock(ctx, lockKey, true); err != nil {
			return err
		}
		if _, exists, err := q.GetAccrualRun(ctx, d); err != nil || exists {
			return err // the date has been totalled: nothing more may be added to it
		}
		loan, err := q.LockLoan(ctx, loanID)
		if err != nil {
			return err
		}
		if !loan.State.Servicing() && !loan.AccrualCatchup {
			return nil
		}
		insts, err := q.Instalments(ctx, loan.ID, loan.ScheduleVersion)
		if err != nil {
			return err
		}
		total, lines := domain.AccrualFor(insts, d)
		if total > 0 {
			inserted, err := q.InsertAccrual(ctx, loan.ID, d, total)
			if err != nil {
				return err
			}
			if inserted {
				for _, l := range lines {
					for i := range insts {
						if insts[i].Seq == l.Seq {
							insts[i].InterestAccrued = l.NewAccrued
						}
					}
				}
				if err := q.SaveInstalments(ctx, loan.ID, loan.ScheduleVersion, insts); err != nil {
					return err
				}
			}
		}
		if loan.AccrualCatchup {
			caughtUp := true
			for _, i := range insts {
				if i.InterestAccrued < i.InterestPaid {
					caughtUp = false
				}
			}
			if caughtUp {
				off := false
				return q.UpdateLoan(ctx, loan.ID, postgres.LoanUpdate{AccrualCatchup: &off})
			}
		}
		return nil
	})
}
