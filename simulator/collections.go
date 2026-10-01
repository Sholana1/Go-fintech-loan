package simulator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Payment is the simulator's record of one inbound payment: money a customer
// paid to the platform through the provider (card, bank transfer, USSD).
type Payment struct {
	Reference   string
	ProviderRef string
	AmountMinor int64
	Currency    string
	Status      string // StatusSuccess, StatusFailed or StatusPending
	Code        string
	PaidAt      time.Time
	// QueryCount is how many status queries were received for the reference.
	QueryCount int
}

// SetPayment records what the provider knows about a payment reference, as
// if the customer had paid (or tried to pay) on the provider's checkout.
func (s *Server) SetPayment(reference string, amountMinor int64, currency, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payments[reference]
	if !ok {
		p = &Payment{Reference: reference, ProviderRef: "CX-" + uuid.NewString()}
		s.payments[reference] = p
	}
	p.AmountMinor, p.Currency, p.Status, p.PaidAt = amountMinor, currency, status, s.opts.Now()
	p.Code = "00"
	if status == StatusFailed {
		p.Code = "DECLINED"
	}
}

// SetPaymentFault makes status queries for a reference fail: "timeout",
// "http500" or "malformed". An empty fault clears it.
func (s *Server) SetPaymentFault(reference, fault string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fault == "" {
		delete(s.paymentFaults, reference)
		return
	}
	s.paymentFaults[reference] = fault
}

// Payment returns a copy of the simulator's record, if any.
func (s *Server) Payment(reference string) (Payment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payments[reference]
	if !ok {
		return Payment{}, false
	}
	return *p, true
}

type paymentResponse struct {
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	ProviderRef string `json:"provider_ref"`
	Code        string `json:"code"`
	PaidAt      string `json:"paid_at,omitempty"`
}

// getPayment is the status query: the provider's own record of a payment.
func (s *Server) getPayment(w http.ResponseWriter, r *http.Request) {
	reference := r.PathValue("reference")
	s.mu.Lock()
	fault := s.paymentFaults[reference]
	p, ok := s.payments[reference]
	var resp paymentResponse
	if ok {
		p.QueryCount++
		resp = paymentResponse{Reference: p.Reference, Status: p.Status, AmountMinor: p.AmountMinor, Currency: p.Currency,
			ProviderRef: p.ProviderRef, Code: p.Code, PaidAt: p.PaidAt.UTC().Format(time.RFC3339)}
	}
	s.mu.Unlock()

	switch fault {
	case "timeout":
		hang(r)
		return
	case "http500":
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	case "malformed":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"reference": "`+reference+`", "status": "SUCC`)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "PAYMENT_NOT_FOUND"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// sandboxPay stands in for the provider's checkout page during manual
// testing: it records a payment for a reference and, when auto-callbacks are
// on, sends the webhook a real provider would send.
func (s *Server) sandboxPay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reference   string `json:"reference"`
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Status      string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reference == "" || req.AmountMinor <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	if req.Currency == "" {
		req.Currency = "NGN"
	}
	switch req.Status {
	case "":
		req.Status = StatusSuccess
	case StatusSuccess, StatusFailed, StatusPending:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_STATUS"})
		return
	}
	s.SetPayment(req.Reference, req.AmountMinor, req.Currency, req.Status)
	writeJSON(w, http.StatusOK, map[string]string{"reference": req.Reference, "status": req.Status})
	if s.opts.AutoCallback && req.Status != StatusPending {
		go func() { _ = s.SendPaymentWebhook(context.Background(), uuid.NewString(), req.Reference) }()
	}
}

// PaymentWebhook is the body of a simulated inbound-payment webhook. It
// deliberately carries no amount or status: the receiver must ask the
// provider (getPayment) what actually happened.
type PaymentWebhook struct {
	EventID    string `json:"event_id"`
	Type       string `json:"type"`
	Reference  string `json:"reference"`
	OccurredAt string `json:"occurred_at"`
}

// SendPaymentWebhook posts a signed webhook for a payment reference to the
// configured URL. Passing the same eventID twice simulates a duplicate
// delivery.
func (s *Server) SendPaymentWebhook(ctx context.Context, eventID, reference string) error {
	body, err := json.Marshal(PaymentWebhook{EventID: eventID, Type: "payment.updated", Reference: reference,
		OccurredAt: s.opts.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	return s.PostPaymentWebhook(ctx, body, strconv.FormatInt(s.opts.Now().Unix(), 10), "")
}

// PostPaymentWebhook posts a raw webhook body. signature, when empty, is
// computed correctly; tests pass a wrong one to simulate a forged webhook.
func (s *Server) PostPaymentWebhook(ctx context.Context, body []byte, timestamp, signature string) error {
	s.mu.Lock()
	target := s.opts.CollectionWebhookURL
	s.mu.Unlock()
	return s.postSigned(ctx, target, body, timestamp, signature)
}

// SetCollectionWebhookURL sets where inbound-payment webhooks are sent.
func (s *Server) SetCollectionWebhookURL(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.CollectionWebhookURL = u
}
