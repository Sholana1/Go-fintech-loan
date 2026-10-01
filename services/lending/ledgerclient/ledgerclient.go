// Package ledgerclient adapts the ledger's Go client to lending's app.Ledger
// port. All of the gRPC mechanics (retries with the same reference, error
// classification) live in the ledger's own client package; this file only
// translates between that package's types and lending's.
package ledgerclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	ledger "bankplatform.internal/services/ledger/ledgerclient"
	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

// Client implements app.Ledger.
type Client struct {
	ledger *ledger.Client
}

// New returns a ledger client. perAttempt bounds each attempt; the caller's
// context bounds the whole call.
func New(conn ledgerv1.LedgerServiceClient, currency string, perAttempt time.Duration, log *slog.Logger, onRetry func(operation string)) *Client {
	return &Client{ledger: ledger.New(conn, currency, perAttempt, log, onRetry)}
}

// translate maps the ledger client's refusals onto the port's error values.
// Anything else is an unknown outcome and is returned unchanged.
func translate(err error) error {
	for from, to := range map[error]error{
		ledger.ErrInsufficientFunds: app.ErrLedgerInsufficientFunds,
		ledger.ErrRejected:          app.ErrLedgerRejected,
		ledger.ErrHoldNotFound:      app.ErrLedgerHoldNotFound,
		ledger.ErrHoldCaptured:      app.ErrLedgerHoldCaptured,
		ledger.ErrHoldNotActive:     app.ErrLedgerHoldNotActive,
		ledger.ErrNotFound:          app.ErrLedgerNotFound,
	} {
		if errors.Is(err, from) {
			return fmt.Errorf("%w: %v", to, err)
		}
	}
	return err
}

func request(req app.PostRequest) ledger.PostRequest {
	lines := make([]ledger.Line, len(req.Lines))
	for i, l := range req.Lines {
		lines[i] = ledger.Line{AccountCode: l.AccountCode, Direction: l.Direction, AmountMinor: l.AmountMinor}
	}
	return ledger.PostRequest{
		Ref:         ledger.Ref{OpType: req.Ref.OpType, OpID: req.Ref.OpID, OpStep: req.Ref.OpStep},
		JournalType: req.JournalType, Lines: lines, BusinessDate: req.BusinessDate, Narrative: req.Narrative,
	}
}

func posted(p ledger.Posted, err error) (app.PostedJournal, error) {
	if err != nil {
		return app.PostedJournal{}, translate(err)
	}
	return app.PostedJournal{ID: p.ID, PostedAt: p.PostedAt, AlreadyPosted: p.AlreadyPosted}, nil
}

func (c *Client) Post(ctx context.Context, req app.PostRequest) (app.PostedJournal, error) {
	return posted(c.ledger.Post(ctx, request(req)))
}

func (c *Client) PlaceHold(ctx context.Context, opType string, opID uuid.UUID, accountCode string, amountMinor int64) error {
	return translate(c.ledger.PlaceHold(ctx, opType, opID, accountCode, amountMinor))
}

func (c *Client) CaptureHold(ctx context.Context, holdOpType string, holdOpID uuid.UUID, req app.PostRequest) (app.PostedJournal, error) {
	return posted(c.ledger.CaptureHold(ctx, holdOpType, holdOpID, request(req)))
}

func (c *Client) ReleaseHold(ctx context.Context, opType string, opID uuid.UUID) error {
	return translate(c.ledger.ReleaseHold(ctx, opType, opID))
}

func (c *Client) Available(ctx context.Context, accountCode string) (int64, error) {
	b, err := c.ledger.Balance(ctx, accountCode)
	return b.Available, translate(err)
}

func (c *Client) Posted(ctx context.Context, accountCode string) (int64, error) {
	b, err := c.ledger.Balance(ctx, accountCode)
	return b.Posted, translate(err)
}

func (c *Client) Journal(ctx context.Context, r app.JournalRef) (app.LedgerJournal, error) {
	j, err := c.ledger.Journal(ctx, ledger.Ref{OpType: r.OpType, OpID: r.OpID, OpStep: r.OpStep})
	if err != nil {
		return app.LedgerJournal{}, translate(err)
	}
	out := app.LedgerJournal{ID: j.ID, Type: j.Type}
	for _, l := range j.Lines {
		out.Lines = append(out.Lines, domain.JournalLine{AccountCode: l.AccountCode, Direction: l.Direction, AmountMinor: l.AmountMinor})
	}
	return out, nil
}

var _ app.Ledger = (*Client)(nil)
