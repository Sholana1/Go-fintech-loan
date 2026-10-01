package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// writeOffPayload records what a write-off removed from the books.
type writeOffPayload struct {
	ActionID  string `json:"action_id"`
	Principal int64  `json:"principal"`
	Interest  int64  `json:"interest"`
	Fees      int64  `json:"fees"`
}

// writeOffIntent builds the write-off journal:
//
//	Dr Loan write-off expense        principal + interest + fees
//	Cr Loans receivable - principal  outstanding principal
//	Cr Loan interest receivable      interest recognised but unpaid
//	Cr Loan fees receivable          fees charged but unpaid
//
// Only interest already recognised in the ledger is written off; interest
// that was earned but not yet accrued was never booked and simply stops.
// Expected-credit-loss provisioning (IFRS 9) is finance's model and is not
// implemented here; this is a direct write-off.
func writeOffIntent(loan domain.Loan, insts []domain.Instalment, actionID uuid.UUID) (domain.PostingIntent, error) {
	var p writeOffPayload
	for _, i := range insts {
		p.Principal += i.PrincipalOutstanding()
		p.Interest += i.InterestAccrued - i.InterestPaid
		p.Fees += i.FeesOutstanding()
	}
	if p.Interest < 0 {
		// Payments are ahead of recognised interest; the accrual job has not
		// caught up. Writing off now would leave the receivable unbalanced.
		return domain.PostingIntent{}, fmt.Errorf("%w: interest accrual is not up to date for this loan; retry after the accrual job has run", domain.ErrConflict)
	}
	total := p.Principal + p.Interest + p.Fees
	if total <= 0 {
		return domain.PostingIntent{}, fmt.Errorf("%w: nothing to write off", domain.ErrConflict)
	}
	p.ActionID = actionID.String()
	payload, err := json.Marshal(p)
	if err != nil {
		return domain.PostingIntent{}, err
	}
	lines := []domain.JournalLine{{AccountCode: contract.AccountLoanWriteOffExpense, Direction: "DEBIT", AmountMinor: total}}
	for _, l := range []struct {
		code   string
		amount int64
	}{
		{contract.AccountLoansPrincipal, p.Principal},
		{contract.AccountLoansInterestReceivable, p.Interest},
		{contract.AccountLoansFeesReceivable, p.Fees},
	} {
		if l.amount > 0 {
			lines = append(lines, domain.JournalLine{AccountCode: l.code, Direction: "CREDIT", AmountMinor: l.amount})
		}
	}
	return domain.PostingIntent{
		ID: uuid.New(), Kind: domain.IntentWriteOff, LoanID: &loan.ID,
		JournalType: contract.JournalLoanWriteOff, Lines: lines, Payload: payload,
	}, nil
}

func (s *Service) applyWriteOff(ctx context.Context, q *postgres.Queries, intent domain.PostingIntent, loan domain.Loan, posted *PostedJournal, rejectReason string, now time.Time) error {
	var p writeOffPayload
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return err
	}
	actionID, err := uuid.Parse(p.ActionID)
	if err != nil {
		return err
	}
	if posted == nil {
		if err := q.FinishAdminAction(ctx, actionID, domain.ActionFailed, now); err != nil {
			return err
		}
		_, err := q.OpenException(ctx, domain.ExceptionPostingRejected, "posting_intent", intent.ID.String(), 0,
			map[string]any{"kind": "WRITE_OFF", "reason": rejectReason, "loan_id": loan.ID.String()})
		return err
	}
	total := p.Principal + p.Interest + p.Fees
	writtenOff := domain.LoanWrittenOff
	var none *time.Time
	if err := q.UpdateLoan(ctx, loan.ID, postgres.LoanUpdate{State: &writtenOff, WrittenOffMinor: &total, NextCollectionAt: &none}); err != nil {
		return err
	}
	if err := q.FinishAdminAction(ctx, actionID, domain.ActionExecuted, now); err != nil {
		return err
	}
	if err := q.Audit(ctx, postgres.SystemActor, "LOAN_WRITTEN_OFF", "loan", loan.ID.String(),
		map[string]any{"action_id": p.ActionID, "principal": p.Principal, "interest": p.Interest, "fees": p.Fees, "journal_id": posted.ID}); err != nil {
		return err
	}
	loan.State = writtenOff
	return s.emitLoan(ctx, q, "loan.written_off", loan, loanEvent{AmountMinor: total, JournalID: posted.ID})
}
