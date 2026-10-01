package httpapi

import (
	"net/http"
	"time"

	"bankplatform.internal/platform/httpx"
)

type loginRequest struct {
	Phone string `json:"phone"`
	PIN   string `json:"pin"`
}

type sessionResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	Subject     string    `json:"subject"`
}

func (h *Handler) customerLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Counted per phone number, before the PIN is checked: an attacker
	// guessing PINs spends the budget whether or not the guesses are right.
	if !h.guard.Allow(w, r, h.login, "customer:"+req.Phone) {
		return
	}
	s, err := h.svc.AuthenticateCustomer(r.Context(), req.Phone, req.PIN)
	if err != nil {
		httpx.WriteError(w, r, h.translate(r, err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sessionResponse{AccessToken: s.Token, ExpiresAt: s.ExpiresAt, Subject: s.Subject.String()})
}

type staffLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) staffLogin(w http.ResponseWriter, r *http.Request) {
	var req staffLoginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !h.guard.Allow(w, r, h.login, "staff:"+req.Email) {
		return
	}
	s, err := h.svc.AuthenticateStaff(r.Context(), req.Email, req.Password)
	if err != nil {
		httpx.WriteError(w, r, h.translate(r, err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sessionResponse{AccessToken: s.Token, ExpiresAt: s.ExpiresAt, Subject: s.Subject.String()})
}
