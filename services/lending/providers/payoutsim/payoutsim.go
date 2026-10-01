// Package payoutsim is the client adapter for the SIMULATED payout provider
// (see package sim). It implements app.PayoutProvider over HTTP and verifies
// the simulator's callback signatures.
//
// Status: UNVERIFIED against any real payment rail or provider. A production
// adapter needs the provider's documentation, sandbox access and
// certification. The behaviours this adapter fixes, which a production
// adapter must preserve:
//
//   - Send carries our reference; the provider must treat it as idempotent.
//   - Only an explicit SUCCESS or FAILED status is an outcome. A transport
//     error, a timeout, a non-200 response, an undecodable body or an
//     unrecognised status is returned as an ERROR, which the caller treats
//     as an unknown outcome. This adapter never converts doubt into failure.
//   - Callbacks are authenticated (package simsig) before they are parsed.
package payoutsim

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"bankplatform.internal/services/lending/app"
)

// ProviderName identifies this adapter in records and metrics.
const ProviderName = "payout-simulator"

// Client implements app.PayoutProvider.
type Client struct {
	baseURL  string
	apiKey   string
	currency string
	http     *http.Client
}

func New(baseURL, apiKey, currency string) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, currency: currency, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) Name() string { return ProviderName }

func (c *Client) do(ctx context.Context, method, path string, in any) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}

var _ app.PayoutProvider = (*Client)(nil)
