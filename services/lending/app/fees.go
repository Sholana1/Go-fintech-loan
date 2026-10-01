package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// lateFeePayload identifies the instalment a late fee belongs to.
type lateFeePayload struct {
	DueDate     string `json:"due_date"`
	AmountMinor int64  `json:"amount_minor"`
}

func (s *Service) applyLateFee(ctx context.Context, q *postgres.Queries, intent domain.PostingIntent, loan domain.Loan, posted *PostedJournal, rejectReason string, now time.Time) error {
	if posted == nil {
		if err := q.CompleteLateFee(ctx, intent.ID, "REJECTED"); err != nil {
			return err
		}
		_, err := q.OpenException(ctx, domain.ExceptionPostingRejected, "posting_intent", intent.ID.String(), 0,
			map[string]any{"kind": "LATE_FEE", "reason": rejectReason, "loan_id": loan.ID.String()})
		return err
	}
	var p lateFeePayload
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return err
	}
	due, err := time.Parse(time.DateOnly, p.DueDate)
	if err != nil {
		return err
	}
	insts, err := q.Instalments(ctx, loan.ID, loan.ScheduleVersion)
	if err != nil {
		return err
	}
	applied := false
	for i := range insts {
		if insts[i].DueDate.Equal(due) {
			insts[i].FeesDue += p.AmountMinor
			applied = true
			break
		}
	}
	if !applied {
		// The schedule was restructured between assessment and posting; the
		// fee is owed on the oldest unpaid instalment instead.
		for i := range insts {
			if !insts[i].FullyPaid() {
				insts[i].FeesDue += p.AmountMinor
				applied = true
				break
			}
		}
	}
	if !applied {
		return fmt.Errorf("late fee for loan %s has no instalment to attach to", loan.ID)
	}
	if err := q.SaveInstalments(ctx, loan.ID, loan.ScheduleVersion, insts); err != nil {
		return err
	}
	if err := q.CompleteLateFee(ctx, intent.ID, "POSTED"); err != nil {
		return err
	}
	if err := q.Audit(ctx, postgres.SystemActor, "LATE_FEE_CHARGED", "loan", loan.ID.String(),
		map[string]any{"amount_minor": p.AmountMinor, "instalment_due_date": p.DueDate, "journal_id": posted.ID}); err != nil {
		return err
	}
	return s.emitLoan(ctx, q, "loan.fee.charged", loan, loanEvent{AmountMinor: p.AmountMinor, JournalID: posted.ID, Detail: "LATE_FEE"})
}
