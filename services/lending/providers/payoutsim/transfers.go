package payoutsim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

type transfer struct {
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	ProviderRef string `json:"provider_ref"`
	Code        string `json:"code"`
}

// outcome maps a provider status to an outcome. An unrecognised status is an
// error: it must not be guessed into success or failure.
func outcome(status string) (domain.ProviderOutcome, error) {
	switch status {
	case "SUCCESS":
		return domain.ProviderSuccess, nil
	case "FAILED":
		return domain.ProviderFailed, nil
	case "PENDING":
		return domain.ProviderPending, nil
	default:
		return "", fmt.Errorf("unrecognised transfer status %q", status)
	}
}

func decodeTransfer(raw []byte, wantReference string) (app.PayoutResult, error) {
	var t transfer
	if err := json.Unmarshal(raw, &t); err != nil {
		return app.PayoutResult{}, fmt.Errorf("transfer response is malformed: %w", err)
	}
	if t.Reference != wantReference {
		return app.PayoutResult{}, fmt.Errorf("transfer response is for reference %q, expected %q", t.Reference, wantReference)
	}
	o, err := outcome(t.Status)
	if err != nil {
		return app.PayoutResult{}, err
	}
	return app.PayoutResult{Outcome: o, ProviderRef: t.ProviderRef, Code: t.Code}, nil
}

func (c *Client) Send(ctx context.Context, in app.PayoutInstruction) (app.PayoutResult, error) {
	status, raw, err := c.do(ctx, http.MethodPost, "/payout/v1/transfers", map[string]any{
		"reference": in.Reference, "amount_minor": in.AmountMinor, "currency": c.currency,
		"bank_code": in.BankCode, "account_number": in.AccountNumber, "narration": in.Narration,
	})
	if err != nil {
		return app.PayoutResult{}, fmt.Errorf("send transfer: %w", err) // outcome unknown
	}
	if status != http.StatusOK {
		// Even a 4xx is treated as unknown here: without the provider's
		// documentation we cannot know which codes guarantee "not processed".
		return app.PayoutResult{}, fmt.Errorf("send transfer returned HTTP %d", status)
	}
	return decodeTransfer(raw, in.Reference)
}

func (c *Client) Query(ctx context.Context, reference string) (app.PayoutResult, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/payout/v1/transfers/"+url.PathEscape(reference), nil)
	if err != nil {
		return app.PayoutResult{}, fmt.Errorf("query transfer: %w", err)
	}
	switch status {
	case http.StatusOK:
		return decodeTransfer(raw, reference)
	case http.StatusNotFound:
		return app.PayoutResult{Outcome: domain.ProviderNotFound, Code: "NOT_FOUND"}, nil
	default:
		return app.PayoutResult{}, fmt.Errorf("query transfer returned HTTP %d", status)
	}
}
