// Package bvnsim is the client adapter for the SIMULATED identity (BVN)
// verification provider (see package simulator). It implements
// app.IdentityVerifier with a real HTTP call.
//
// Status: UNVERIFIED against any real provider. It is NOT an integration
// with NIBSS or any identity vendor: a production adapter needs the
// provider's documentation, sandbox credentials and certification (a launch
// dependency; see docs/integrations/README.md). The service refuses to start
// with this adapter outside local and test environments.
//
// What a production adapter must preserve:
//
//   - Only an explicit MATCH, MISMATCH or NOT_FOUND is an answer. A timeout,
//     a transport error, a non-200 status or an undecodable body is returned
//     as an error, and registration is refused (fail closed). Doubt is never
//     turned into a match.
//   - The call carries our reference, so the same check can be repeated
//     without being billed or logged as a second enquiry.
//   - The BVN travels only in the request body over TLS, never in a URL, a
//     header or a log line.
package bvnsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"bankplatform.internal/services/identity/app"
)

// ProviderName identifies this adapter in records.
const ProviderName = "bvn-simulator"

// Client implements app.IdentityVerifier.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New returns a client. timeout bounds one verification end to end; it is
// the longest a registration request waits for the provider.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: timeout}}
}

type verificationRequest struct {
	RequestRef  string `json:"request_ref"`
	BVN         string `json:"bvn"`
	FullName    string `json:"full_name"`
	DateOfBirth string `json:"date_of_birth"`
}

type verificationResponse struct {
	VerificationRef string `json:"verification_ref"`
	Result          string `json:"result"`
}

// VerifyBVN makes one HTTP call: POST /kyc/v1/bvn-verifications.
//
// There is no retry here. The caller is a customer waiting on a registration
// request; if the provider does not answer in time the customer is told to
// try again, which repeats the call with their consent and attention.
func (c *Client) VerifyBVN(ctx context.Context, ref, bvn, fullName string, dateOfBirth time.Time) (app.Verification, error) {
	body, err := json.Marshal(verificationRequest{RequestRef: ref, BVN: bvn, FullName: fullName, DateOfBirth: dateOfBirth.Format(time.DateOnly)})
	if err != nil {
		return app.Verification{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/kyc/v1/bvn-verifications", bytes.NewReader(body))
	if err != nil {
		return app.Verification{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Idempotency-Key", ref)

	resp, err := c.http.Do(req) // the external call
	if err != nil {
		// The error text of net/http includes the URL but never the body,
		// so the BVN cannot leak through it.
		return app.Verification{}, fmt.Errorf("bvn verification: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return app.Verification{}, fmt.Errorf("bvn verification: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return app.Verification{}, fmt.Errorf("bvn verification returned HTTP %d", resp.StatusCode)
	}
	var out verificationResponse
	if err := json.Unmarshal(raw, &out); err != nil || out.VerificationRef == "" {
		return app.Verification{}, errors.New("bvn verification response is malformed")
	}
	v := app.Verification{Provider: ProviderName, ProviderRef: out.VerificationRef}
	switch out.Result {
	case "MATCH":
		v.Outcome = app.OutcomeMatch
	case "MISMATCH":
		v.Outcome = app.OutcomeMismatch
	case "NOT_FOUND":
		v.Outcome = app.OutcomeNotFound
	default:
		return app.Verification{}, fmt.Errorf("bvn verification returned unrecognised result %q", out.Result)
	}
	return v, nil
}

var _ app.IdentityVerifier = (*Client)(nil)
