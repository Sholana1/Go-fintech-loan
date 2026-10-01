package app

import (
	"context"
	"time"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// applyRecovery records money received on a written-off loan.
func (s *Service) applyRecovery(ctx context.Context, q *postgres.Queries, intent domain.PostingIntent, loan domain.Loan, posted *PostedJournal, rejectReason string, now time.Time) error {
	rep, err := q.RepaymentByIntent(ctx, intent.ID)
	if err != nil {
		return err
	}
	if posted == nil {
		return q.CompleteRepayment(ctx, rep.ID, domain.RepaymentRejected, nil, rejectReason, now)
	}
	recovered := loan.RecoveredMinor + rep.AppliedMinor
	update := postgres.LoanUpdate{RecoveredMinor: &recovered}
	if recovered >= loan.WrittenOffMinor {
		closed := domain.LoanClosed
		update.State, update.ClosedAt = &closed, &now
		loan.State = closed
	}
	if err := q.UpdateLoan(ctx, loan.ID, update); err != nil {
		return err
	}
	if err := q.CompleteRepayment(ctx, rep.ID, domain.RepaymentAllocated, &posted.ID, "", now); err != nil {
		return err
	}
	if err := q.Audit(ctx, postgres.Actor{Kind: "customer", ID: rep.CustomerID.String()}, "RECOVERY_RECEIVED", "loan", loan.ID.String(),
		map[string]any{"repayment_id": rep.ID.String(), "amount_minor": rep.AppliedMinor, "journal_id": posted.ID}); err != nil {
		return err
	}
	return s.emitLoan(ctx, q, "loan.recovery.received", loan, loanEvent{AmountMinor: rep.AppliedMinor, JournalID: posted.ID, RepaymentID: rep.ID.String()})
}
