package app

import (
	"context"
	"fmt"
	"time"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// applyDisbursement marks the loan ACTIVE. This is the only place a loan
// becomes disbursed, and it runs only with a posted journal in hand: "queued"
// is never reported as "disbursed". The loans table enforces the same thing
// with a CHECK (an ACTIVE loan must have disbursed_at and a journal id).
func (s *Service) applyDisbursement(ctx context.Context, q *postgres.Queries, loan domain.Loan, posted *PostedJournal, rejectReason string, now time.Time) ([]func(), error) {
	if loan.State != domain.LoanPendingDisbursement {
		return nil, fmt.Errorf("loan %s is %s, expected PENDING_DISBURSEMENT", loan.ID, loan.State)
	}
	app, err := q.GetApplication(ctx, loan.ApplicationID)
	if err != nil {
		return nil, err
	}

	if posted == nil {
		failed := domain.LoanDisbursementFailed
		if err := q.UpdateLoan(ctx, loan.ID, postgres.LoanUpdate{State: &failed}); err != nil {
			return nil, err
		}
		if _, err := q.TransitionApplication(ctx, app.ID, []domain.ApplicationState{domain.AppAccepted},
			postgres.ApplicationUpdate{To: domain.AppDisbursementFailed, Reasons: []string{domain.ReasonDisbursementRejected}}, now); err != nil {
			return nil, err
		}
		if err := s.cancelPendingPayout(ctx, q, loan.ID, now); err != nil {
			return nil, err
		}
		// A refused disbursement needs a person: the customer accepted a
		// loan they did not receive.
		if _, err := q.OpenException(ctx, domain.ExceptionPostingRejected, "loan", loan.ID.String(), loan.PrincipalMinor,
			map[string]any{"kind": "DISBURSEMENT", "reason": rejectReason}); err != nil {
			return nil, err
		}
		if err := q.Audit(ctx, postgres.SystemActor, "DISBURSEMENT_REJECTED", "loan", loan.ID.String(), map[string]any{"reason": rejectReason}); err != nil {
			return nil, err
		}
		loan.State = failed
		return []func(){func() { s.metrics.ReconException(domain.ExceptionPostingRejected) }},
			s.emitLoan(ctx, q, "loan.disbursement_failed", loan, loanEvent{Detail: rejectReason})
	}

	active := domain.LoanActive
	update := postgres.LoanUpdate{State: &active, DisbursedAt: &posted.PostedAt, JournalID: &posted.ID}
	if loan.AutoDebitAuthorised {
		// First scheduled collection: the morning of the first due date.
		insts, err := q.Instalments(ctx, loan.ID, loan.ScheduleVersion)
		if err != nil {
			return nil, err
		}
		first := bizdate.StartOf(insts[0].DueDate)
		firstPtr := &first
		update.NextCollectionAt = &firstPtr
	}
	if err := q.UpdateLoan(ctx, loan.ID, update); err != nil {
		return nil, err
	}
	if _, err := q.TransitionApplication(ctx, app.ID, []domain.ApplicationState{domain.AppAccepted},
		postgres.ApplicationUpdate{To: domain.AppDisbursed}, now); err != nil {
		return nil, err
	}
	payoutID, err := s.releasePendingPayout(ctx, q, loan.ID, now)
	if err != nil {
		return nil, err
	}
	if err := q.Audit(ctx, postgres.SystemActor, "LOAN_DISBURSED", "loan", loan.ID.String(), map[string]any{"journal_id": posted.ID}); err != nil {
		return nil, err
	}
	loan.State = active
	if err := s.emitLoan(ctx, q, "loan.disbursed", loan, loanEvent{PrincipalMinor: loan.PrincipalMinor, JournalID: posted.ID}); err != nil {
		return nil, err
	}

	offer, err := q.LatestOffer(ctx, loan.ApplicationID)
	if err != nil {
		return nil, err
	}
	after := []func(){func() {
		// The five-minute measure (plan section 1.A): system time is decision
		// time (T0->T1) plus disbursement time (T2->T3). Customer thinking
		// time between T1 and T2 is not ours and is excluded.
		if app.DecidedAt != nil && offer.AcceptedAt != nil {
			acceptToPosted := posted.PostedAt.Sub(*offer.AcceptedAt)
			s.metrics.LoanDisbursed(acceptToPosted, app.DecidedAt.Sub(app.SubmittedAt)+acceptToPosted)
		}
	}}
	// A payout, if any, is now READY with next_attempt_at = now. It is sent
	// by the payout driver, not here: the customer's acceptance response
	// must not wait on an external provider.
	_ = payoutID
	return after, nil
}
