package ledgerclient

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "bankplatform.internal/gen/bank/common/v1"
	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/services/ledger/contract"
)

// Client calls the ledger.
type Client struct {
	ledger   ledgerv1.LedgerServiceClient
	currency string
	log      *slog.Logger
	// onRetry is called for every retried call, for a retry metric.
	onRetry func(operation string)
	policy  grpcx.RetryPolicy
}

// New returns a ledger client for one currency. perAttempt bounds each
// attempt; the caller's context bounds the whole call. onRetry may be nil.
func New(ledger ledgerv1.LedgerServiceClient, currency string, perAttempt time.Duration, log *slog.Logger, onRetry func(operation string)) *Client {
	return &Client{
		ledger: ledger, currency: currency, log: log, onRetry: onRetry,
		policy: grpcx.RetryPolicy{Attempts: 3, BaseBackoff: 50 * time.Millisecond, PerAttempt: perAttempt},
	}
}

func (c *Client) retry(ctx context.Context, operation string, call func(ctx context.Context) error) error {
	p := c.policy
	p.OnRetry = func(attempt int, err error) {
		c.log.WarnContext(ctx, "retrying ledger call with the same reference", "operation", operation, "attempt", attempt, "error", err.Error())
		if c.onRetry != nil {
			c.onRetry(operation)
		}
	}
	return classify(grpcx.RetryIdempotent(ctx, p, call))
}

// classify maps a ledger error onto the sentinel errors using the ledger's
// stable reasons, never its message text. An error it does not recognise as
// a refusal is returned unchanged: the outcome is unknown.
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch contract.ReasonOf(err) {
	case contract.ReasonInsufficientFunds:
		return fmt.Errorf("%w: %v", ErrInsufficientFunds, err)
	case contract.ReasonHoldNotFound:
		return fmt.Errorf("%w: %v", ErrHoldNotFound, err)
	case contract.ReasonHoldCaptured:
		return fmt.Errorf("%w: %v", ErrHoldCaptured, err)
	case contract.ReasonHoldNotActive:
		return fmt.Errorf("%w: %v", ErrHoldNotActive, err)
	case contract.ReasonJournalNotFound, contract.ReasonAccountNotFound:
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case contract.ReasonAccountNotActive, contract.ReasonNotAuthorised, contract.ReasonInvalid, contract.ReasonUnbalanced,
		contract.ReasonCurrencyMismatch, contract.ReasonRefReused, contract.ReasonAccountConflict:
		return fmt.Errorf("%w: %v", ErrRejected, err)
	}
	// No reason attached. A definite client-side refusal is still a refusal;
	// everything else (UNAVAILABLE, DEADLINE_EXCEEDED, ABORTED, INTERNAL) is
	// an unknown outcome.
	switch status.Code(err) {
	case codes.InvalidArgument, codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition:
		return fmt.Errorf("%w: %v", ErrRejected, err)
	}
	return err
}

func (c *Client) lines(in []Line) ([]*ledgerv1.Line, error) {
	out := make([]*ledgerv1.Line, len(in))
	for i, l := range in {
		var d ledgerv1.Direction
		switch l.Direction {
		case Debit:
			d = ledgerv1.Direction_DIRECTION_DEBIT
		case Credit:
			d = ledgerv1.Direction_DIRECTION_CREDIT
		default:
			return nil, fmt.Errorf("journal line %d has direction %q", i, l.Direction)
		}
		out[i] = &ledgerv1.Line{AccountCode: l.AccountCode, Direction: d, Amount: &commonv1.Money{MinorUnits: l.AmountMinor, Currency: c.currency}}
	}
	return out, nil
}

func ref(r Ref) *ledgerv1.PostingRef {
	return &ledgerv1.PostingRef{OpType: r.OpType, OpId: r.OpID.String(), OpStep: r.OpStep}
}

// Post posts one journal. It is idempotent on req.Ref.
func (c *Client) Post(ctx context.Context, req PostRequest) (Posted, error) {
	lines, err := c.lines(req.Lines)
	if err != nil {
		return Posted{}, err
	}
	in := &ledgerv1.PostJournalRequest{Ref: ref(req.Ref), JournalType: req.JournalType, Lines: lines, Narrative: req.Narrative}
	if req.BusinessDate != nil {
		in.BusinessDate = req.BusinessDate.Format(time.DateOnly)
	}
	var out *ledgerv1.PostJournalResponse
	err = c.retry(ctx, "PostJournal", func(ctx context.Context) error {
		var err error
		out, err = c.ledger.PostJournal(ctx, in)
		return err
	})
	if err != nil {
		return Posted{}, err
	}
	return Posted{ID: out.GetJournalId(), PostedAt: out.GetPostedAt().AsTime(), AlreadyPosted: out.GetAlreadyPosted()}, nil
}

// PlaceHold reserves funds on an account. It is idempotent on (opType, opID).
func (c *Client) PlaceHold(ctx context.Context, opType string, opID uuid.UUID, accountCode string, amountMinor int64) error {
	return c.retry(ctx, "PlaceHold", func(ctx context.Context) error {
		_, err := c.ledger.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{
			Ref: &ledgerv1.HoldRef{OpType: opType, OpId: opID.String()}, AccountCode: accountCode,
			Amount: &commonv1.Money{MinorUnits: amountMinor, Currency: c.currency},
		})
		return err
	})
}

// CaptureHold closes a hold and posts the journal that spends it, in one
// ledger transaction. It is idempotent on req.Ref.
func (c *Client) CaptureHold(ctx context.Context, holdOpType string, holdOpID uuid.UUID, req PostRequest) (Posted, error) {
	lines, err := c.lines(req.Lines)
	if err != nil {
		return Posted{}, err
	}
	var out *ledgerv1.CaptureHoldResponse
	err = c.retry(ctx, "CaptureHold", func(ctx context.Context) error {
		var err error
		out, err = c.ledger.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{
			Hold: &ledgerv1.HoldRef{OpType: holdOpType, OpId: holdOpID.String()},
			Ref:  ref(req.Ref), JournalType: req.JournalType, Lines: lines, Narrative: req.Narrative,
		})
		return err
	})
	if err != nil {
		return Posted{}, err
	}
	return Posted{ID: out.GetJournalId(), PostedAt: out.GetPostedAt().AsTime(), AlreadyPosted: out.GetAlreadyCaptured()}, nil
}

// ReleaseHold gives held funds back. It is idempotent.
func (c *Client) ReleaseHold(ctx context.Context, opType string, opID uuid.UUID) error {
	return c.retry(ctx, "ReleaseHold", func(ctx context.Context) error {
		_, err := c.ledger.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{Hold: &ledgerv1.HoldRef{OpType: opType, OpId: opID.String()}})
		return err
	})
}

// Balance returns an account's balance. It is for display and for sizing a
// request; the ledger still decides, under lock, whether a debit is allowed.
func (c *Client) Balance(ctx context.Context, accountCode string) (Balance, error) {
	var out *ledgerv1.GetBalanceResponse
	err := c.retry(ctx, "GetBalance", func(ctx context.Context) error {
		var err error
		out, err = c.ledger.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountCode: accountCode})
		return err
	})
	if err != nil {
		return Balance{}, err
	}
	return Balance{Posted: out.GetPosted().GetMinorUnits(), Held: out.GetHeld().GetMinorUnits(), Available: out.GetAvailable().GetMinorUnits()}, nil
}

// Journal returns the journal posted under a reference, for reconciliation.
func (c *Client) Journal(ctx context.Context, r Ref) (Journal, error) {
	var out *ledgerv1.GetJournalResponse
	err := c.retry(ctx, "GetJournal", func(ctx context.Context) error {
		var err error
		out, err = c.ledger.GetJournal(ctx, &ledgerv1.GetJournalRequest{Ref: ref(r)})
		return err
	})
	if err != nil {
		return Journal{}, err
	}
	j := Journal{ID: out.GetJournalId(), Type: out.GetJournalType()}
	for _, l := range out.GetLines() {
		dir := Credit
		if l.GetDirection() == ledgerv1.Direction_DIRECTION_DEBIT {
			dir = Debit
		}
		j.Lines = append(j.Lines, Line{AccountCode: l.GetAccountCode(), Direction: dir, AmountMinor: l.GetAmount().GetMinorUnits()})
	}
	return j, nil
}
