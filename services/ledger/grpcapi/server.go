// Package grpcapi is the ledger's gRPC transport. It converts protobuf
// messages to domain commands, calls the application service with the
// caller's workload identity, and maps domain errors to gRPC statuses.
// It contains no business rules.
package grpcapi

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "bankplatform.internal/gen/bank/common/v1"
	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/platform/money"
	"bankplatform.internal/services/ledger/app"
	"bankplatform.internal/services/ledger/domain"
	"bankplatform.internal/services/ledger/postgres"
)

// Server implements ledgerv1.LedgerServiceServer.
type Server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	svc *app.Service
	log *slog.Logger
}

func NewServer(svc *app.Service, log *slog.Logger) *Server {
	return &Server{svc: svc, log: log}
}

func (s *Server) OpenAccount(ctx context.Context, req *ledgerv1.OpenAccountRequest) (*ledgerv1.OpenAccountResponse, error) {
	if req.GetKind() != ledgerv1.AccountKind_ACCOUNT_KIND_CUSTOMER_DEPOSIT {
		return nil, s.toStatus(ctx, domain.ErrInvalid)
	}
	owner, err := uuid.Parse(req.GetOwnerId())
	if err != nil {
		return nil, s.toStatus(ctx, domain.ErrInvalid)
	}
	existed, err := s.svc.OpenCustomerDeposit(ctx, grpcx.Caller(ctx), req.GetCode(), money.Currency(req.GetCurrency()), owner)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.OpenAccountResponse{Code: req.GetCode(), AlreadyExisted: existed}, nil
}

func (s *Server) PostJournal(ctx context.Context, req *ledgerv1.PostJournalRequest) (*ledgerv1.PostJournalResponse, error) {
	cmd, err := toPostCommand(req.GetRef(), req.GetJournalType(), req.GetLines(), req.GetBusinessDate(), req.GetNarrative())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	j, err := s.svc.PostJournal(ctx, grpcx.Caller(ctx), cmd)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.PostJournalResponse{
		JournalId:     j.ID,
		PostedAt:      timestamppb.New(j.PostedAt),
		BusinessDate:  j.BusinessDate.Format(time.DateOnly),
		AlreadyPosted: j.AlreadyPosted,
	}, nil
}

func (s *Server) PlaceHold(ctx context.Context, req *ledgerv1.PlaceHoldRequest) (*ledgerv1.PlaceHoldResponse, error) {
	ref, err := toHoldRef(req.GetRef())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	amount, err := toAmount(req.GetAmount())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	cmd := postgres.HoldCommand{Ref: ref, AccountCode: req.GetAccountCode(), Amount: amount}
	if req.GetExpiresAt() != nil {
		t := req.GetExpiresAt().AsTime()
		cmd.ExpiresAt = &t
	}
	res, err := s.svc.PlaceHold(ctx, grpcx.Caller(ctx), cmd)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.PlaceHoldResponse{HoldId: res.ID.String(), Status: holdStatusToProto(res.Status), AlreadyPlaced: res.AlreadyPlaced}, nil
}

func (s *Server) CaptureHold(ctx context.Context, req *ledgerv1.CaptureHoldRequest) (*ledgerv1.CaptureHoldResponse, error) {
	hold, err := toHoldRef(req.GetHold())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	post, err := toPostCommand(req.GetRef(), req.GetJournalType(), req.GetLines(), "", req.GetNarrative())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	j, err := s.svc.CaptureHold(ctx, grpcx.Caller(ctx), postgres.CaptureCommand{Hold: hold, Post: post})
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.CaptureHoldResponse{JournalId: j.ID, PostedAt: timestamppb.New(j.PostedAt), AlreadyCaptured: j.AlreadyPosted}, nil
}

func (s *Server) ReleaseHold(ctx context.Context, req *ledgerv1.ReleaseHoldRequest) (*ledgerv1.ReleaseHoldResponse, error) {
	ref, err := toHoldRef(req.GetHold())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	st, already, err := s.svc.ReleaseHold(ctx, grpcx.Caller(ctx), ref)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.ReleaseHoldResponse{Status: holdStatusToProto(st), AlreadyClosed: already}, nil
}

func (s *Server) GetBalance(ctx context.Context, req *ledgerv1.GetBalanceRequest) (*ledgerv1.GetBalanceResponse, error) {
	b, err := s.svc.GetBalance(ctx, grpcx.Caller(ctx), req.GetAccountCode())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	cur := string(b.Currency)
	return &ledgerv1.GetBalanceResponse{
		Posted:    &commonv1.Money{MinorUnits: b.Posted, Currency: cur},
		Held:      &commonv1.Money{MinorUnits: b.Held, Currency: cur},
		Available: &commonv1.Money{MinorUnits: b.Available, Currency: cur},
		Version:   b.Version,
	}, nil
}

func (s *Server) GetJournal(ctx context.Context, req *ledgerv1.GetJournalRequest) (*ledgerv1.GetJournalResponse, error) {
	ref, err := toPostingRef(req.GetRef())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	j, err := s.svc.GetJournal(ctx, grpcx.Caller(ctx), ref)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	resp := &ledgerv1.GetJournalResponse{
		JournalId:    j.ID,
		JournalType:  j.Type,
		PostedAt:     timestamppb.New(j.PostedAt),
		BusinessDate: j.BusinessDate.Format(time.DateOnly),
		CreatedBy:    j.CreatedBy,
	}
	for _, l := range j.Lines {
		resp.Lines = append(resp.Lines, &ledgerv1.Line{
			AccountCode: l.AccountCode,
			Direction:   directionToProto(l.Direction),
			Amount:      &commonv1.Money{MinorUnits: l.Amount.Minor(), Currency: string(l.Amount.Currency())},
		})
	}
	return resp, nil
}
