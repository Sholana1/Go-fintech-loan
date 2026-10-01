package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// RestructureParams are the parameters of a RESTRUCTURE action.
type RestructureParams struct {
	NewTenorMonths int `json:"new_tenor_months"`
}

// ProposeAdminAction records a maker's proposal to write off or restructure
// a loan. Nothing changes on the loan until a different person approves it.
func (s *Service) ProposeAdminAction(ctx context.Context, makerID uuid.UUID, kind domain.AdminActionKind, loanID uuid.UUID, params json.RawMessage, reason string) (domain.AdminAction, error) {
	reason = strings.TrimSpace(reason)
	if len(reason) < 10 {
		return domain.AdminAction{}, fmt.Errorf("%w: a reason of at least 10 characters is required", domain.ErrValidation)
	}
	loan, err := s.store.GetLoan(ctx, loanID)
	if err != nil {
		return domain.AdminAction{}, err
	}
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	if err := s.checkAdminAction(kind, loan, params); err != nil {
		return domain.AdminAction{}, err
	}
	action := domain.AdminAction{
		ID: uuid.New(), Kind: kind, LoanID: loanID, Params: params, Reason: reason,
		State: domain.ActionProposed, MakerID: makerID, CreatedAt: s.now(),
	}
	err = s.store.InTx(ctx, func(q *postgres.Queries) error {
		if err := q.InsertAdminAction(ctx, action); err != nil {
			return err
		}
		return q.Audit(ctx, postgres.Actor{Kind: "staff", ID: makerID.String()}, "ADMIN_ACTION_PROPOSED", "loan", loanID.String(),
			map[string]any{"action_id": action.ID.String(), "kind": string(kind), "reason": reason})
	})
	return action, err
}

// checkAdminAction validates an action against the loan's current state and
// the product's limits. It is run at proposal and again at approval, because
// the loan may have changed in between.
func (s *Service) checkAdminAction(kind domain.AdminActionKind, loan domain.Loan, params json.RawMessage) error {
	switch kind {
	case domain.ActionWriteOff:
		if loan.State != domain.LoanInArrears {
			return fmt.Errorf("%w: only a loan in arrears can be written off (loan is %s)", domain.ErrConflict, loan.State)
		}
		if loan.DaysPastDue < s.product.WriteOffMinDaysPastDue {
			return fmt.Errorf("%w: write-off requires at least %d days past due (loan has %d)", domain.ErrForbidden, s.product.WriteOffMinDaysPastDue, loan.DaysPastDue)
		}
	case domain.ActionRestructure:
		if !loan.State.Servicing() {
			return fmt.Errorf("%w: only a loan being serviced can be restructured (loan is %s)", domain.ErrConflict, loan.State)
		}
		var p RestructureParams
		if err := json.Unmarshal(params, &p); err != nil {
			return fmt.Errorf("%w: restructure parameters are invalid", domain.ErrValidation)
		}
		if p.NewTenorMonths < 1 || p.NewTenorMonths > s.product.RestructureMaxTenorMonths {
			return fmt.Errorf("%w: new tenor must be between 1 and %d months", domain.ErrValidation, s.product.RestructureMaxTenorMonths)
		}
	default:
		return fmt.Errorf("%w: unknown action kind", domain.ErrValidation)
	}
	return nil
}

// RejectAdminAction records a checker's refusal.
func (s *Service) RejectAdminAction(ctx context.Context, checkerID, actionID uuid.UUID, note string) (domain.AdminAction, error) {
	return s.decideAdminAction(ctx, checkerID, actionID, note, false)
}

// ApproveAdminAction records a checker's approval and executes the action.
func (s *Service) ApproveAdminAction(ctx context.Context, checkerID, actionID uuid.UUID, note string) (domain.AdminAction, error) {
	return s.decideAdminAction(ctx, checkerID, actionID, note, true)
}

func (s *Service) decideAdminAction(ctx context.Context, checkerID, actionID uuid.UUID, note string, approve bool) (domain.AdminAction, error) {
	now, today := s.now(), s.today()
	var intentID uuid.UUID
	err := s.store.InTx(ctx, func(q *postgres.Queries) error {
		intentID = uuid.Nil
		peek, err := q.GetAdminAction(ctx, actionID)
		if err != nil {
			return err
		}
		// Loan first, then the action row: consistent with every other
		// loan-level operation.
		loan, err := q.LockLoan(ctx, peek.LoanID)
		if err != nil {
			return err
		}
		action, err := q.LockAdminAction(ctx, actionID)
		if err != nil {
			return err
		}
		if action.State != domain.ActionProposed {
			return fmt.Errorf("%w: action is %s", domain.ErrConflict, action.State)
		}
		if action.MakerID == checkerID {
			return fmt.Errorf("%w: the approver must be a different person from the proposer", domain.ErrForbidden)
		}
		actor := postgres.Actor{Kind: "staff", ID: checkerID.String()}

		if !approve {
			if err := q.DecideAdminAction(ctx, action.ID, domain.ActionRejected, checkerID, note, nil, now); err != nil {
				return err
			}
			return q.Audit(ctx, actor, "ADMIN_ACTION_REJECTED", "loan", loan.ID.String(), map[string]any{"action_id": action.ID.String()})
		}

		if err := s.checkAdminAction(action.Kind, loan, action.Params); err != nil {
			return err
		}
		if pending, err := q.HasPendingIntent(ctx, loan.ID); err != nil {
			return err
		} else if pending {
			return domain.ErrOperationInProgress
		}
		insts, err := q.Instalments(ctx, loan.ID, loan.ScheduleVersion)
		if err != nil {
			return err
		}

		switch action.Kind {
		case domain.ActionWriteOff:
			intent, err := writeOffIntent(loan, insts, action.ID)
			if err != nil {
				return err
			}
			if err := q.InsertIntent(ctx, intent, now); err != nil {
				return err
			}
			if err := q.DecideAdminAction(ctx, action.ID, domain.ActionApproved, checkerID, note, &intent.ID, now); err != nil {
				return err
			}
			intentID = intent.ID

		case domain.ActionRestructure:
			var p RestructureParams
			if err := json.Unmarshal(action.Params, &p); err != nil {
				return err
			}
			restructured, err := domain.Restructure(insts, loan.MonthlyRateBps, p.NewTenorMonths, today)
			if err != nil {
				return fmt.Errorf("%w: %v", domain.ErrConflict, err)
			}
			version := loan.ScheduleVersion + 1
			if err := q.InsertInstalments(ctx, loan.ID, version, restructured); err != nil {
				return err
			}
			active, zero, bucket, yes := domain.LoanActive, 0, s.product.Bucket(0), true
			update := postgres.LoanUpdate{State: &active, ScheduleVersion: &version, DaysPastDue: &zero, ArrearsBucket: &bucket, Restructured: &yes}
			if loan.AutoDebitAuthorised {
				next := nextDueStart(restructured, today)
				update.NextCollectionAt = &next
			}
			if err := q.UpdateLoan(ctx, loan.ID, update); err != nil {
				return err
			}
			if err := q.DecideAdminAction(ctx, action.ID, domain.ActionApproved, checkerID, note, nil, now); err != nil {
				return err
			}
			if err := q.FinishAdminAction(ctx, action.ID, domain.ActionExecuted, now); err != nil {
				return err
			}
			loan.State = active
			// The previous arrears are recorded in the event and the audit
			// trail: a restructure does not erase that the loan was overdue.
			if err := s.emitLoan(ctx, q, "loan.restructured", loan, loanEvent{Detail: fmt.Sprintf("previous_days_past_due=%d", loan.DaysPastDue)}); err != nil {
				return err
			}
		}
		return q.Audit(ctx, actor, "ADMIN_ACTION_APPROVED", "loan", loan.ID.String(),
			map[string]any{"action_id": action.ID.String(), "kind": string(action.Kind), "maker_id": action.MakerID.String(),
				"previous_days_past_due": loan.DaysPastDue, "previous_schedule_version": loan.ScheduleVersion})
	})
	if err != nil {
		return domain.AdminAction{}, err
	}
	if intentID != uuid.Nil {
		if err := s.ProcessIntent(ctx, intentID); err != nil {
			s.log.WarnContext(ctx, "write-off posting deferred", "action_id", actionID.String(), "error", err.Error())
		}
	}
	return s.store.GetAdminAction(ctx, actionID)
}

// OpenAdminActions lists actions awaiting approval.
func (s *Service) OpenAdminActions(ctx context.Context) ([]domain.AdminAction, error) {
	return s.store.OpenAdminActions(ctx, 100)
}
