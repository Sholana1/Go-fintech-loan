package simulator

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Callback is the body of a simulated provider callback.
type Callback struct {
	EventID     string `json:"event_id"`
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	ProviderRef string `json:"provider_ref"`
	Code        string `json:"code"`
	OccurredAt  string `json:"occurred_at"`
}

// Sign returns the signature header value for a callback body.
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// SendCallback posts a signed callback for a transfer to the configured URL.
// statusOverride, when not empty, is sent instead of the transfer's real
// status (to simulate a provider that contradicts itself). Passing the same
// eventID twice simulates a duplicate delivery.
func (s *Server) SendCallback(ctx context.Context, eventID, reference, statusOverride string) error {
	s.mu.Lock()
	t, ok := s.transfers[reference]
	cb := Callback{EventID: eventID, Reference: reference, OccurredAt: s.opts.Now().UTC().Format(time.RFC3339)}
	if ok {
		cb.Status, cb.ProviderRef, cb.Code = t.Status, t.ProviderRef, t.Code
	}
	s.mu.Unlock()
	if statusOverride != "" {
		cb.Status = statusOverride
	}
	body, err := json.Marshal(cb)
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(s.opts.Now().Unix(), 10)
	if strings.HasPrefix(reference, TransferReferencePrefix) {
		return s.PostTransferCallback(ctx, body, ts, "")
	}
	return s.PostCallback(ctx, body, ts, "")
}

// TransferReferencePrefix marks references of customer transfers. Callbacks
// for them go to Options.TransferCallbackURL.
const TransferReferencePrefix = "tr-"

// PostTransferCallback posts a raw callback body to the transfer callback
// URL. signature, when empty, is computed correctly.
func (s *Server) PostTransferCallback(ctx context.Context, body []byte, timestamp, signature string) error {
	s.mu.Lock()
	target := s.opts.TransferCallbackURL
	s.mu.Unlock()
	return s.postSigned(ctx, target, body, timestamp, signature)
}

// SetTransferCallbackURL sets where callbacks for customer transfers go.
func (s *Server) SetTransferCallbackURL(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.TransferCallbackURL = u
}

// PostCallback posts a raw callback body. signature, when empty, is computed
// correctly; tests pass a wrong one to simulate a forged callback.
func (s *Server) PostCallback(ctx context.Context, body []byte, timestamp, signature string) error {
	s.mu.Lock()
	target := s.opts.CallbackURL
	s.mu.Unlock()
	return s.postSigned(ctx, target, body, timestamp, signature)
}

// postSigned delivers a signed notification to target.
func (s *Server) postSigned(ctx context.Context, target string, body []byte, timestamp, signature string) error {
	if target == "" {
		return fmt.Errorf("simulator: no callback URL configured")
	}
	if signature == "" {
		signature = Sign(s.opts.CallbackSecret, timestamp, body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sim-Timestamp", timestamp)
	req.Header.Set("X-Sim-Signature", signature)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("callback rejected: %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// SetCallbackURL sets where callbacks are sent (tests start the receiver
// after the simulator).
func (s *Server) SetCallbackURL(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.CallbackURL = u
}
