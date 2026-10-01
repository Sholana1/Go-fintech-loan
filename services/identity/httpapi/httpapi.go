// Package httpapi is identity's public REST surface: registration and login.
// Handlers decode, call the application service, and translate errors to the
// stable API error codes. They contain no business rules.
package httpapi

import (
	"log/slog"
	"net/http"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/services/identity/app"
)

// Config configures the handler.
type Config struct {
	Logger *slog.Logger
	// Guard rate-limits sign-in attempts. Nil disables limiting. It is abuse
	// protection only: the PIN lockout that protects an account is enforced
	// in PostgreSQL by the application service.
	Guard *ratelimit.Guard
	// LoginLimit is the sign-in budget per phone number or staff email.
	LoginLimit ratelimit.Limit
}

type Handler struct {
	svc   *app.Service
	log   *slog.Logger
	guard *ratelimit.Guard
	login ratelimit.Limit
}

// New returns the identity REST handler with the standard middleware.
func New(svc *app.Service, cfg Config) http.Handler {
	h := &Handler{svc: svc, log: cfg.Logger, guard: cfg.Guard, login: cfg.LoginLimit}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/customers", h.register)
	mux.HandleFunc("POST /v1/sessions", h.customerLogin)
	mux.HandleFunc("POST /v1/staff/sessions", h.staffLogin)
	return httpx.Chain(mux, httpx.Recover(cfg.Logger), httpx.WithRequestID, httpx.AccessLog(cfg.Logger))
}
