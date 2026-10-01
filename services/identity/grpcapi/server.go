// Package grpcapi is identity's internal gRPC transport.
package grpcapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	identityv1 "bankplatform.internal/gen/bank/identity/v1"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/services/identity/app"
	"bankplatform.internal/services/identity/domain"
)

type Server struct {
	identityv1.UnimplementedIdentityServiceServer
	svc *app.Service
	log *slog.Logger
}

func NewServer(svc *app.Service, log *slog.Logger) *Server { return &Server{svc: svc, log: log} }

func (s *Server) GetCustomer(ctx context.Context, req *identityv1.GetCustomerRequest) (*identityv1.GetCustomerResponse, error) {
	id, err := uuid.Parse(req.GetCustomerId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "customer_id must be a UUID")
	}
	c, err := s.svc.GetCustomer(ctx, grpcx.Caller(ctx), id)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	resp := &identityv1.GetCustomerResponse{
		CustomerId: c.ID.String(), KycTier: int32(c.KYCTier), FullName: c.FullName,
		DateOfBirth: c.DateOfBirth.Format(time.DateOnly), DepositAccountCode: c.DepositAccountCode,
	}
	switch c.Status {
	case domain.CustomerActive:
		resp.Status = identityv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE
	case domain.CustomerBlocked:
		resp.Status = identityv1.CustomerStatus_CUSTOMER_STATUS_BLOCKED
	}
	switch c.KYCStatus {
	case domain.KYCPending:
		resp.KycStatus = identityv1.KycStatus_KYC_STATUS_PENDING
	case domain.KYCVerified:
		resp.KycStatus = identityv1.KycStatus_KYC_STATUS_VERIFIED
	case domain.KYCRejected:
		resp.KycStatus = identityv1.KycStatus_KYC_STATUS_REJECTED
	}
	return resp, nil
}

func (s *Server) VerifyCustomerPin(ctx context.Context, req *identityv1.VerifyCustomerPinRequest) (*identityv1.VerifyCustomerPinResponse, error) {
	id, err := uuid.Parse(req.GetCustomerId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "customer_id must be a UUID")
	}
	ok, locked, err := s.svc.VerifyPIN(ctx, grpcx.Caller(ctx), id, req.GetPin())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &identityv1.VerifyCustomerPinResponse{Verified: ok, Locked: locked}, nil
}

func (s *Server) GetCreditBureauSubject(ctx context.Context, req *identityv1.GetCreditBureauSubjectRequest) (*identityv1.GetCreditBureauSubjectResponse, error) {
	id, err := uuid.Parse(req.GetCustomerId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "customer_id must be a UUID")
	}
	sub, err := s.svc.GetBureauSubject(ctx, grpcx.Caller(ctx), id, req.GetConsentRef())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &identityv1.GetCreditBureauSubjectResponse{Bvn: sub.BVN, FullName: sub.FullName, DateOfBirth: sub.DateOfBirth.Format(time.DateOnly)}, nil
}

func (s *Server) ResolveRecipient(ctx context.Context, req *identityv1.ResolveRecipientRequest) (*identityv1.ResolveRecipientResponse, error) {
	r, err := s.svc.ResolveRecipient(ctx, grpcx.Caller(ctx), req.GetPhone())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &identityv1.ResolveRecipientResponse{CustomerId: r.CustomerID.String(), DisplayName: r.DisplayName, DepositAccountCode: r.DepositAccountCode}, nil
}

// toStatus maps domain errors to gRPC codes:
// NOT_FOUND (unknown customer), PERMISSION_DENIED (caller not allowed),
// INVALID_ARGUMENT (bad input), INTERNAL (anything else; logged, not returned).
func (s *Server) toStatus(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return status.Error(codes.NotFound, "customer not found")
	case errors.Is(err, domain.ErrNotAuthorised):
		return status.Error(codes.PermissionDenied, "caller is not authorised for this RPC")
	case errors.Is(err, domain.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "cancelled")
	default:
		s.log.ErrorContext(ctx, "identity internal error", "error", err.Error())
		return status.Error(codes.Internal, "internal error")
	}
}
