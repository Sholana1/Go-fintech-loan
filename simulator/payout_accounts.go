package simulator

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// SetAccountName sets the name returned by name enquiry for an account.
func (s *Server) SetAccountName(accountNumber, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names[accountNumber] = name
}

// SetPayoutBehaviour overrides the behaviour code ("90".."99", or "" for
// success) for an account number.
func (s *Server) SetPayoutBehaviour(accountNumber, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payoutByAcct[accountNumber] = code
}

func (s *Server) behaviour(accountNumber string) string {
	if code, ok := s.payoutByAcct[accountNumber]; ok {
		return code
	}
	if len(accountNumber) >= 2 {
		return accountNumber[len(accountNumber)-2:]
	}
	return ""
}

func (s *Server) resolveAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BankCode      string `json:"bank_code"`
		AccountNumber string `json:"account_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccountNumber == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_REQUEST"})
		return
	}
	s.mu.Lock()
	code := s.behaviour(req.AccountNumber)
	name, named := s.names[req.AccountNumber]
	s.mu.Unlock()

	switch code {
	case "97":
		hang(r)
		return
	case "98":
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "ACCOUNT_NOT_FOUND"})
		return
	case "99":
		name, named = "SOMEONE ELSE ENTIRELY", true
	}
	if !named {
		name = "ADA TEST CUSTOMER"
	}
	writeJSON(w, http.StatusOK, map[string]string{"account_name": name, "enquiry_ref": "NE-" + uuid.NewString()})
}
