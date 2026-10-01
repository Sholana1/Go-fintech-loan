package simulator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Transfer statuses used by the simulated payout provider.
const (
	StatusSuccess = "SUCCESS"
	StatusFailed  = "FAILED"
	StatusPending = "PENDING"
)

// Transfer is the simulator's record of one payout.
type Transfer struct {
	Reference     string
	ProviderRef   string
	AmountMinor   int64
	BankCode      string
	AccountNumber string
	Status        string
	Code          string
	Settled       bool
	CreatedAt     time.Time
	// SendCount is how many times a send was received for this reference.
	// The platform must never make it exceed 1 per intended transfer.
	SendCount  int
	queryCount int
	// successAfterQueries turns a PENDING transfer into SUCCESS after that
	// many status queries (delayed success).
	successAfterQueries int
}

type transferRequest struct {
	Reference     string `json:"reference"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	Narration     string `json:"narration"`
}

type transferResponse struct {
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	ProviderRef string `json:"provider_ref"`
	Code        string `json:"code"`
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reference == "" || req.AmountMinor <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}

	s.mu.Lock()
	code := s.behaviour(req.AccountNumber)
	if code == "95" {
		// The provider fails before recording anything.
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	}
	t, exists := s.transfers[req.Reference]
	if exists {
		// Idempotent on reference: never a second payment.
		t.SendCount++
	} else {
		t = &Transfer{
			Reference: req.Reference, ProviderRef: "PX-" + uuid.NewString(), AmountMinor: req.AmountMinor,
			BankCode: req.BankCode, AccountNumber: req.AccountNumber, CreatedAt: s.opts.Now(), SendCount: 1,
			Status: StatusSuccess, Code: "00",
		}
		switch code {
		case "90":
			t.Status, t.Code = StatusFailed, "ACCOUNT_CLOSED"
		case "92":
			t.Status, t.Code = StatusFailed, "REJECTED_BY_BENEFICIARY_BANK"
		case "93":
			t.Status, t.Code, t.successAfterQueries = StatusPending, "IN_PROGRESS", 2
		case "96":
			t.Status, t.Code = StatusPending, "IN_PROGRESS"
		}
		s.transfers[req.Reference] = t
	}
	resp := transferResponse{Reference: t.Reference, Status: t.Status, ProviderRef: t.ProviderRef, Code: t.Code}
	final := t.Status != StatusPending
	s.mu.Unlock()

	switch code {
	case "91", "92":
		// The transfer was processed; the response never arrives.
		hang(r)
		return
	case "94":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status": "SUCC`)
		return
	}
	writeJSON(w, http.StatusOK, resp)
	if s.opts.AutoCallback && final && !exists {
		go func() { _ = s.SendCallback(context.Background(), uuid.NewString(), resp.Reference, "") }()
	}
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	reference := r.PathValue("reference")
	s.mu.Lock()
	t, ok := s.transfers[reference]
	var resp transferResponse
	if ok {
		t.queryCount++
		if t.Status == StatusPending && t.successAfterQueries > 0 && t.queryCount >= t.successAfterQueries {
			t.Status, t.Code = StatusSuccess, "00"
		}
		resp = transferResponse{Reference: t.Reference, Status: t.Status, ProviderRef: t.ProviderRef, Code: t.Code}
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "TRANSFER_NOT_FOUND"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Resolve sets the final status of a transfer (for behaviour 96).
func (s *Server) Resolve(reference, status, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.transfers[reference]; ok {
		t.Status, t.Code = status, code
	}
}

// Transfer returns a copy of the simulator's record, if any.
func (s *Server) Transfer(reference string) (Transfer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.transfers[reference]
	if !ok {
		return Transfer{}, false
	}
	return *t, true
}

// TransferCount returns how many distinct transfers exist.
func (s *Server) TransferCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.transfers)
}

// SeedTransfer records a transfer as if a send had been received and
// processed. Tests use it to model "the request reached the provider but we
// never saw the response".
func (s *Server) SeedTransfer(reference string, amountMinor int64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transfers[reference] = &Transfer{
		Reference: reference, ProviderRef: "PX-" + uuid.NewString(), AmountMinor: amountMinor,
		Status: status, Code: "00", CreatedAt: s.opts.Now(), SendCount: 1,
	}
}
