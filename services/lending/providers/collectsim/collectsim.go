// Package collectsim is the client adapter for the SIMULATED inbound-payment
// provider (see package simulator). It implements app.PaymentVerifier with a
// real HTTP call and verifies the simulator's webhook signatures.
//
// Status: UNVERIFIED against any real provider; refused outside local and
// test environments. The production counterpart for a provider whose public
// API could be verified is package paystack.
//
// The behaviours this adapter fixes, which every adapter must preserve:
//
//   - Only an explicit SUCCESS, FAILED or PENDING status is an answer, and
//     "no such payment" is NOT_FOUND. A transport error, a timeout, an
//     unexpected HTTP status, an undecodable body or an unrecognised status
//     is returned as an ERROR, which the caller treats as "unknown, ask
//     again". Doubt is never turned into success or into failure.
//   - The response must be about the reference that was asked for.
//   - A webhook is authenticated before it is parsed, and yields only a
//     reference: it says when to ask, not what happened.
package collectsim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/providers/simsig"
)

// ProviderName identifies this adapter in records and metrics.
const ProviderName = "collection-simulator"

// Client implements app.PaymentVerifier.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func New(baseURL, apiKey string) *Client {
	// The per-call deadline comes from the caller's context; this timeout
	// is only a backstop.
	return &Client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) Name() string { return ProviderName }

type payment struct {
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	ProviderRef string `json:"provider_ref"`
	Code        string `json:"code"`
}

// VerifyPayment makes one HTTP call: GET /collections/v1/payments/{reference}.
// It is a read, so it is safe to repeat; the caller schedules repeats.
func (c *Client) VerifyPayment(ctx context.Context, reference string) (app.PaymentStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/collections/v1/payments/"+url.PathEscape(reference), nil)
	if err != nil {
		return app.PaymentStatus{}, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req) // the external call
	if err != nil {
		return app.PaymentStatus{}, fmt.Errorf("verify payment: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return app.PaymentStatus{}, fmt.Errorf("verify payment: read response: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return app.PaymentStatus{Outcome: domain.ProviderNotFound, Code: "NOT_FOUND"}, nil
	default:
		return app.PaymentStatus{}, fmt.Errorf("verify payment returned HTTP %d", resp.StatusCode)
	}
	var p payment
	if err := json.Unmarshal(raw, &p); err != nil {
		return app.PaymentStatus{}, fmt.Errorf("verify payment response is malformed: %w", err)
	}
	if p.Reference != reference {
		return app.PaymentStatus{}, fmt.Errorf("verify payment response is for reference %q, expected %q", p.Reference, reference)
	}
	out := app.PaymentStatus{AmountMinor: p.AmountMinor, Currency: p.Currency, ProviderRef: p.ProviderRef, Code: p.Code}
	switch p.Status {
	case "SUCCESS":
		out.Outcome = domain.ProviderSuccess
	case "FAILED":
		out.Outcome = domain.ProviderFailed
	case "PENDING":
		out.Outcome = domain.ProviderPending
	default:
		return app.PaymentStatus{}, fmt.Errorf("unrecognised payment status %q", p.Status)
	}
	return out, nil
}

var _ app.PaymentVerifier = (*Client)(nil)

// Webhooks authenticates and decodes inbound-payment webhooks.
type Webhooks struct {
	Secret string
}

// VerifyPaymentWebhook authenticates a webhook and extracts the reference.
func (v Webhooks) VerifyPaymentWebhook(h http.Header, body []byte, now time.Time) (app.PaymentNotice, error) {
	if err := simsig.Authenticate(v.Secret, h, body, now); err != nil {
		return app.PaymentNotice{}, err
	}
	var n struct {
		EventID   string `json:"event_id"`
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(body, &n); err != nil || n.EventID == "" || n.Reference == "" {
		return app.PaymentNotice{}, errors.Join(domain.ErrValidation, errors.New("webhook body is malformed"))
	}
	return app.PaymentNotice{Provider: ProviderName, EventID: n.EventID, Reference: n.Reference, Raw: body}, nil
}
