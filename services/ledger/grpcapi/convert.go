package grpcapi

import (
	"time"

	"github.com/google/uuid"

	commonv1 "bankplatform.internal/gen/bank/common/v1"
	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/money"
	"bankplatform.internal/services/ledger/domain"
	"bankplatform.internal/services/ledger/postgres"
)

func toPostCommand(ref *ledgerv1.PostingRef, journalType string, lines []*ledgerv1.Line, businessDate, narrative string) (postgres.PostCommand, error) {
	pr, err := toPostingRef(ref)
	if err != nil {
		return postgres.PostCommand{}, err
	}
	cmd := postgres.PostCommand{Ref: pr, JournalType: journalType, Narrative: narrative}
	for _, l := range lines {
		amount, err := toAmount(l.GetAmount())
		if err != nil {
			return postgres.PostCommand{}, err
		}
		var dir domain.Direction
		switch l.GetDirection() {
		case ledgerv1.Direction_DIRECTION_DEBIT:
			dir = domain.Debit
		case ledgerv1.Direction_DIRECTION_CREDIT:
			dir = domain.Credit
		}
		cmd.Lines = append(cmd.Lines, domain.Line{AccountCode: l.GetAccountCode(), Direction: dir, Amount: amount})
	}
	if businessDate != "" {
		d, err := time.Parse(time.DateOnly, businessDate)
		if err != nil {
			return postgres.PostCommand{}, domain.ErrInvalid
		}
		cmd.BusinessDate = &d
	}
	return cmd, nil
}

func toPostingRef(r *ledgerv1.PostingRef) (domain.PostingRef, error) {
	id, err := uuid.Parse(r.GetOpId())
	if err != nil {
		return domain.PostingRef{}, domain.ErrInvalid
	}
	return domain.PostingRef{OpType: r.GetOpType(), OpID: id, OpStep: r.GetOpStep()}, nil
}

func toHoldRef(r *ledgerv1.HoldRef) (domain.HoldRef, error) {
	id, err := uuid.Parse(r.GetOpId())
	if err != nil {
		return domain.HoldRef{}, domain.ErrInvalid
	}
	return domain.HoldRef{OpType: r.GetOpType(), OpID: id}, nil
}

func toAmount(m *commonv1.Money) (money.Amount, error) {
	if m == nil {
		return money.Amount{}, domain.ErrInvalid
	}
	a, err := money.New(m.GetMinorUnits(), money.Currency(m.GetCurrency()))
	if err != nil {
		return money.Amount{}, domain.ErrInvalid
	}
	return a, nil
}

func directionToProto(d domain.Direction) ledgerv1.Direction {
	if d == domain.Debit {
		return ledgerv1.Direction_DIRECTION_DEBIT
	}
	return ledgerv1.Direction_DIRECTION_CREDIT
}

func holdStatusToProto(s domain.HoldStatus) ledgerv1.HoldStatus {
	switch s {
	case domain.HoldActive:
		return ledgerv1.HoldStatus_HOLD_STATUS_ACTIVE
	case domain.HoldCaptured:
		return ledgerv1.HoldStatus_HOLD_STATUS_CAPTURED
	case domain.HoldReleased:
		return ledgerv1.HoldStatus_HOLD_STATUS_RELEASED
	case domain.HoldExpired:
		return ledgerv1.HoldStatus_HOLD_STATUS_EXPIRED
	}
	return ledgerv1.HoldStatus_HOLD_STATUS_UNSPECIFIED
}
