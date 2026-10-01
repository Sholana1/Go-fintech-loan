package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// ClassifyArrears recomputes days past due and the arrears bucket of every
// loan being serviced, assesses late fees that have fallen due, and makes
// sure a collection is scheduled where money is owed. It returns false if
// any loan had to be skipped because another posting was in flight, in which
// case the caller runs it again later.
func (s *Service) ClassifyArrears(ctx context.Context, today time.Time) (complete bool, err error) {
	complete = true
	after := uuid.Nil
	for {
		ids, err := s.store.ServicingLoanIDs(ctx, after, 500)
		if err != nil {
			return false, err
		}
		if len(ids) == 0 {
			return complete, nil
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			skipped, err := s.classifyLoan(ctx, id, today)
			if err != nil {
				return false, fmt.Errorf("loan %s: %w", id, err)
			}
			if skipped {
				complete = false
			}
		}
		after = ids[len(ids)-1]
	}
}

func (s *Service) classifyLoan(ctx context.Context, loanID uuid.UUID, today time.Time) (skipped bool, err error) {
	var feeIntent uuid.UUID
	now := s.now()
	err = s.store.InTx(ctx, func(q *postgres.Queries) error {
		feeIntent, skipped = uuid.Nil, false
		loan, err := q.LockLoan(ctx, loanID)
		if err != nil {
			return err
		}
		if !loan.State.Servicing() {
			return nil
		}
		insts, err := q.Instalments(ctx, loan.ID, loan.ScheduleVersion)
		if err != nil {
			return err
		}

		dpd := domain.DaysPastDue(insts, today)
		bucket := s.product.Bucket(dpd)
		state := domain.LoanActive
		if dpd > 0 {
			state = domain.LoanInArrears
		}
		if err := q.UpdateLoan(ctx, loan.ID, postgres.LoanUpdate{State: &state, DaysPastDue: &dpd, ArrearsBucket: &bucket}); err != nil {
			return err
		}
		if state != loan.State || bucket != loan.ArrearsBucket {
			if err := q.Audit(ctx, postgres.SystemActor, "ARREARS_CLASSIFIED", "loan", loan.ID.String(),
				map[string]any{"days_past_due": dpd, "bucket": bucket, "state": string(state)}); err != nil {
				return err
			}
			loan.State = state
			if err := s.emitLoan(ctx, q, "loan.arrears.changed", loan, loanEvent{Detail: bucket}); err != nil {
				return err
			}
		}

		if loan.AutoDebitAuthorised && domain.AmountDue(insts, today) > 0 {
			if err := q.ScheduleCollectionIfUnset(ctx, loan.ID, now); err != nil {
				return err
			}
		}

		if s.product.LateFeeMinor <= 0 {
			return nil
		}
		for _, cand := range domain.LateFeeCandidates(insts, today, s.product.LateFeeGraceDays) {
			exists, err := q.LateFeeExists(ctx, loan.ID, cand.DueDate)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			// One posting per loan at a time. If a repayment is in flight,
			// leave the fee for the next pass rather than charge a fee on an
			// instalment that is being paid right now.
			pending, err := q.HasPendingIntent(ctx, loan.ID)
			if err != nil {
				return err
			}
			if pending {
				skipped = true
				return nil
			}
			payload, err := json.Marshal(lateFeePayload{DueDate: cand.DueDate.Format(time.DateOnly), AmountMinor: s.product.LateFeeMinor})
			if err != nil {
				return err
			}
			id := uuid.New()
			if err := q.InsertIntent(ctx, domain.PostingIntent{
				ID: id, Kind: domain.IntentLateFee, LoanID: &loan.ID, JournalType: contract.JournalLoanFee, Payload: payload,
				Lines: []domain.JournalLine{
					{AccountCode: contract.AccountLoansFeesReceivable, Direction: "DEBIT", AmountMinor: s.product.LateFeeMinor},
					{AccountCode: contract.AccountFeeIncome, Direction: "CREDIT", AmountMinor: s.product.LateFeeMinor},
				},
			}, now); err != nil {
				return err
			}
			if _, err := q.InsertLateFee(ctx, uuid.New(), loan.ID, cand.DueDate, s.product.LateFeeMinor, id); err != nil {
				return err
			}
			feeIntent = id
			// At most one fee per pass: the next candidate is assessed once
			// this posting has been applied.
			return nil
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrOperationInProgress) {
			return true, nil
		}
		return false, err
	}
	if feeIntent != uuid.Nil {
		if err := s.ProcessIntent(ctx, feeIntent); err != nil {
			s.log.WarnContext(ctx, "late fee posting deferred", "loan_id", loanID.String(), "error", err.Error())
		}
	}
	return skipped, nil
}
