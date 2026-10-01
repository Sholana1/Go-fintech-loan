package paystack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

// transferData is the part of Paystack's transfer object this adapter reads
// (TransferCreateResponse and TransferVerifyResponse share these fields).
type transferData struct {
	Reference    string `json:"reference"`
	Status       string `json:"status"`
	TransferCode string `json:"transfer_code"`
	Amount       int64  `json:"amount"`
	Currency     string `json:"currency"`
}

// result turns a transfer object into a payout result, refusing to use one
// that is not about the transfer we asked about.
func (c *Client) result(raw json.RawMessage, wantReference string, wantAmount int64) (app.PayoutResult, error) {
	var t transferData
	if err := json.Unmarshal(raw, &t); err != nil {
		return app.PayoutResult{}, fmt.Errorf("paystack: transfer object is malformed: %w", err)
	}
	if t.Reference != wantReference {
		return app.PayoutResult{}, fmt.Errorf("paystack: transfer object is for reference %q, expected %q", t.Reference, wantReference)
	}
	if wantAmount > 0 && t.Amount != wantAmount {
		return app.PayoutResult{}, fmt.Errorf("paystack: transfer %s is for amount %d, expected %d", wantReference, t.Amount, wantAmount)
	}
	if t.Currency != "" && t.Currency != c.currency {
		return app.PayoutResult{}, fmt.Errorf("paystack: transfer %s is in %s, expected %s", wantReference, t.Currency, c.currency)
	}
	outcome, code, err := transferOutcome(t.Status)
	if err != nil {
		return app.PayoutResult{}, err
	}
	return app.PayoutResult{Outcome: outcome, ProviderRef: t.TransferCode, Code: code}, nil
}

// Send pays out one transfer. Paystack needs two calls:
//
//  1. POST /transferrecipient  register the destination account; returns a
//     recipient_code. No money moves.
//  2. POST /transfer           send from our Paystack balance to that
//     recipient, carrying OUR reference.
//
// What each failure means is different, and that difference is the point:
//
//   - Step 1 fails in any way: we know for certain that no transfer exists,
//     because step 2 was never attempted. That is a clean FAILED outcome;
//     the caller releases the customer's funds.
//   - Step 2 gets no usable answer (timeout, connection reset, 5xx,
//     unexpected body, even a 4xx): the transfer may or may not exist at
//     Paystack. That is returned as an ERROR, which the caller records as
//     UNKNOWN and resolves by Query on the same reference. It is never
//     retried with a new reference.
//
// Idempotency: the reference is our own, fixed before this is called. The
// specification says of it: "To ensure idempotency, you need to provide a
// unique identifier for the request." It must be lower-case alphanumeric
// with only '-' and '_', at least 16 characters; our references are built
// that way ("lp-" + UUID).
func (c *Client) Send(ctx context.Context, in app.PayoutInstruction) (app.PayoutResult, error) {
	recipient, err := c.createRecipient(ctx, in)
	if err != nil {
		// Nothing was sent. Say so plainly instead of leaving doubt.
		return app.PayoutResult{Outcome: domain.ProviderFailed, Code: "RECIPIENT_NOT_CREATED"}, nil
	}

	r, err := c.call(ctx, http.MethodPost, "/transfer", nil, map[string]any{
		"source":    "balance",
		"amount":    in.AmountMinor, // kobo
		"recipient": recipient,
		"reason":    in.Narration,
		"reference": in.Reference,
		"currency":  c.currency,
	})
	if err != nil {
		return app.PayoutResult{}, err // outcome unknown
	}
	if r.HTTPStatus != http.StatusOK || !r.OK {
		return app.PayoutResult{}, r.unexpected("initiate transfer") // outcome unknown
	}
	return c.result(r.Data, in.Reference, in.AmountMinor)
}

// createRecipient registers the destination account and returns its
// recipient code. It moves no money and may be repeated freely.
func (c *Client) createRecipient(ctx context.Context, in app.PayoutInstruction) (string, error) {
	r, err := c.call(ctx, http.MethodPost, "/transferrecipient", nil, map[string]any{
		"type":           "nuban",
		"name":           in.AccountName,
		"account_number": in.AccountNumber,
		"bank_code":      in.BankCode,
		"currency":       c.currency,
	})
	if err != nil {
		return "", err
	}
	if (r.HTTPStatus != http.StatusCreated && r.HTTPStatus != http.StatusOK) || !r.OK {
		return "", r.unexpected("create transfer recipient")
	}
	var data struct {
		RecipientCode string `json:"recipient_code"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil || data.RecipientCode == "" {
		return "", errors.New("paystack create transfer recipient: response is malformed")
	}
	return data.RecipientCode, nil
}
