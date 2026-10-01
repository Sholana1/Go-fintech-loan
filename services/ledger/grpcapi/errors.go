package grpcapi

import (
	"context"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/ledger/app"
	"bankplatform.internal/services/ledger/contract"
)

// toStatus maps a domain error to a gRPC status with a stable reason.
//
//	INVALID_ARGUMENT     malformed or unbalanced request (never retry)
//	PERMISSION_DENIED    caller lacks the posting right (never retry)
//	NOT_FOUND            unknown account, hold or journal
//	FAILED_PRECONDITION  state forbids it: insufficient funds, inactive
//	                     account, closed hold, reference reused (never retry
//	                     unchanged)
//	ABORTED              lock contention after bounded retries (retry with the
//	                     same reference)
//	DEADLINE_EXCEEDED    caller's deadline passed (outcome unknown: retry with
//	                     the same reference to learn it)
//	INTERNAL             anything else; details are logged, not returned
func (s *Server) toStatus(ctx context.Context, err error) error {
	reason := app.Reason(err)
	var code codes.Code
	switch reason {
	case contract.ReasonInvalid, contract.ReasonUnbalanced, contract.ReasonCurrencyMismatch:
		code = codes.InvalidArgument
	case contract.ReasonNotAuthorised:
		code = codes.PermissionDenied
	case contract.ReasonAccountNotFound, contract.ReasonHoldNotFound, contract.ReasonJournalNotFound:
		code = codes.NotFound
	case contract.ReasonInsufficientFunds, contract.ReasonAccountNotActive, contract.ReasonRefReused,
		contract.ReasonHoldNotActive, contract.ReasonHoldCaptured, contract.ReasonAccountConflict:
		code = codes.FailedPrecondition
	default:
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return status.Error(codes.DeadlineExceeded, "deadline exceeded; retry with the same reference")
		case errors.Is(err, context.Canceled):
			return status.Error(codes.Canceled, "request cancelled")
		}
		switch pgxutil.Code(err) {
		case pgxutil.CodeSerializationFailure, pgxutil.CodeDeadlockDetected, pgxutil.CodeLockNotAvailable, pgxutil.CodeQueryCanceled:
			s.log.WarnContext(ctx, "ledger transaction contention", "sqlstate", pgxutil.Code(err))
			return status.Error(codes.Aborted, "transaction contention; retry with the same reference")
		}
		s.log.ErrorContext(ctx, "ledger internal error", "error", err.Error())
		return status.Error(codes.Internal, "internal error")
	}

	st := status.New(code, err.Error())
	if detailed, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: contract.ErrorDomain}); derr == nil {
		st = detailed
	}
	return st.Err()
}
