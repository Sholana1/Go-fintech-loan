package simulator

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
)

// KYC results returned by the simulated identity provider.
const (
	KYCMatch    = "MATCH"
	KYCMismatch = "MISMATCH"
	KYCNotFound = "NOT_FOUND"
)

// SetKYCBehaviour overrides the answer for one BVN. behaviour is a KYC
// result, or "timeout", "http500" or "malformed".
func (s *Server) SetKYCBehaviour(bvn, behaviour string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kycByBVN[bvn] = behaviour
}

// KYCCalls returns how many verification requests carried a request ref.
func (s *Server) KYCCalls(requestRef string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kycCalls[requestRef]
}

// kycDefault selects behaviour from the last four digits of the BVN:
//
//	...0000  NOT_FOUND
//	...1111  MISMATCH (name or date of birth differs)
//	...9999  the provider never answers (timeout)
//	...8888  malformed response
//	...7777  HTTP 500
//	other    MATCH
func kycDefault(bvn string) string {
	if len(bvn) < 4 {
		return KYCNotFound
	}
	switch bvn[len(bvn)-4:] {
	case "0000":
		return KYCNotFound
	case "1111":
		return KYCMismatch
	case "9999":
		return "timeout"
	case "8888":
		return "malformed"
	case "7777":
		return "http500"
	default:
		return KYCMatch
	}
}

type kycRequest struct {
	RequestRef  string `json:"request_ref"`
	BVN         string `json:"bvn"`
	FullName    string `json:"full_name"`
	DateOfBirth string `json:"date_of_birth"`
}

type kycResponse struct {
	VerificationRef string `json:"verification_ref"`
	Result          string `json:"result"`
}

func (s *Server) verifyBVN(w http.ResponseWriter, r *http.Request) {
	var req kycRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RequestRef == "" || req.BVN == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	s.mu.Lock()
	s.kycCalls[req.RequestRef]++
	behaviour, ok := s.kycByBVN[req.BVN]
	if !ok {
		behaviour = kycDefault(req.BVN)
	}
	cached := s.kycChecks[req.RequestRef]
	s.mu.Unlock()

	switch behaviour {
	case "timeout":
		hang(r)
		return
	case "http500":
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "INTERNAL"})
		return
	case "malformed":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"verification_ref": "x", "result": `)
		return
	}
	// The same request reference always returns the same verification.
	if cached == nil {
		cached, _ = json.Marshal(kycResponse{VerificationRef: "KV-" + uuid.NewString(), Result: behaviour})
		s.mu.Lock()
		s.kycChecks[req.RequestRef] = cached
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(cached)
}
