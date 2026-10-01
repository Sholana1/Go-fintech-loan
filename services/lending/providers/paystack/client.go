package paystack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProviderName identifies this adapter in records and metrics.
const ProviderName = "paystack"

// DefaultBaseURL is the server named in Paystack's OpenAPI specification.
const DefaultBaseURL = "https://api.paystack.co"

// Config configures the adapter.
type Config struct {
	// BaseURL defaults to DefaultBaseURL. Tests point it at a local server.
	BaseURL string
	// SecretKey is the integration's secret key (sk_test_... or sk_live_...).
	// It comes from the environment or a secret manager, never from source.
	SecretKey string
	// Currency of the transfers this adapter sends (for example "NGN").
	Currency string
	// HTTPClient is optional. The per-call deadline always comes from the
	// caller's context; the client's own timeout is only a backstop.
	HTTPClient *http.Client
}

// Client talks to Paystack.
type Client struct {
	baseURL  string
	key      string
	currency string
	http     *http.Client
}

func New(cfg Config) (*Client, error) {
	if cfg.SecretKey == "" {
		return nil, errors.New("paystack: a secret key is required")
	}
	if cfg.Currency == "" {
		return nil, errors.New("paystack: a currency is required")
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("paystack: invalid base URL %q", cfg.BaseURL)
	}
	// Plain HTTP would send the secret key in clear text. It is allowed only
	// to a loopback address, which is what tests use.
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return nil, errors.New("paystack: the base URL must use https")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{baseURL: base, key: cfg.SecretKey, currency: cfg.Currency, http: hc}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *Client) Name() string { return ProviderName }

// reply is one HTTP response from Paystack: the status code and the
// envelope every Paystack response uses ({"status", "message", "data"}).
type reply struct {
	HTTPStatus int
	// OK is the envelope's "status" flag: whether Paystack carried out the
	// request. It says nothing about whether a transfer succeeded.
	OK      bool            `json:"status"`
	Message string          `json:"message"`
	Code    string          `json:"code"`
	Data    json.RawMessage `json:"data"`
	Meta    json.RawMessage `json:"meta"`
}

// call performs one HTTP request. THIS IS WHERE THE EXTERNAL CALL IS MADE:
// every operation in this package goes through here.
//
// It returns an error only when no usable answer was obtained (transport
// failure, timeout, unreadable or undecodable body). An HTTP error status
// with a well-formed envelope is returned as a reply, because what it means
// depends on the operation.
//
// There are no retries here. Whether a call may be repeated is a decision
// for the caller, which knows whether the operation moves money.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, in any) (reply, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return reply{}, err
		}
		body = bytes.NewReader(raw)
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// net/http's error names the URL but never the headers, so the key
		// cannot leak through it.
		return reply{}, fmt.Errorf("paystack %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return reply{}, fmt.Errorf("paystack %s %s: read response: %w", method, path, err)
	}
	out := reply{HTTPStatus: resp.StatusCode}
	if err := json.Unmarshal(raw, &out); err != nil {
		return reply{}, fmt.Errorf("paystack %s %s: HTTP %d with a body that is not the documented envelope", method, path, resp.StatusCode)
	}
	return out, nil
}

// unexpected describes a reply the operation has no rule for.
func (r reply) unexpected(operation string) error {
	return fmt.Errorf("paystack %s: HTTP %d (%s)", operation, r.HTTPStatus, r.Message)
}

// refused reports whether Paystack understood the request and declined it,
// as opposed to failing to process it. 401/403 (our credentials) and 429
// (rate limit) are excluded: those say nothing about the request itself.
func (r reply) refused() bool {
	return r.HTTPStatus >= 400 && r.HTTPStatus < 500 &&
		r.HTTPStatus != http.StatusUnauthorized && r.HTTPStatus != http.StatusForbidden &&
		r.HTTPStatus != http.StatusTooManyRequests
}
