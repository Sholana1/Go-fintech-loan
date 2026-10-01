// Package httpx holds the REST plumbing shared by services that expose a
// public API: a stable JSON error shape, strict JSON decoding, bearer-token
// authentication, panic recovery and access logging.
//
// It contains no business rules. Middleware order used by every service
// (outermost first): Recover -> RequestID -> AccessLog -> Authenticate ->
// handler. Each is small and does one thing.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/authn"
)

// MaxBodyBytes caps request bodies. Loan and payment requests are tiny; a
// large body is either a mistake or abuse.
const MaxBodyBytes = 64 << 10

// APIError is the stable error contract of every REST endpoint. Code is
// machine-readable and never changes meaning; Message is for humans.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// Errorf builds an APIError.
func Errorf(status int, code, format string, args ...any) *APIError {
	return &APIError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

type requestIDKey struct{}

// RequestID returns the request's correlation ID.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey{}).(string)
	return s
}

// WriteJSON writes v with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the error envelope. Unknown errors become a generic 500
// so internal details never reach clients; the detail is logged by the caller.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = &APIError{Status: http.StatusInternalServerError, Code: "INTERNAL", Message: "internal error"}
	}
	WriteJSON(w, apiErr.Status, map[string]any{
		"error": map[string]any{
			"code":       apiErr.Code,
			"message":    apiErr.Message,
			"request_id": RequestID(r.Context()),
		},
	})
}

// DecodeJSON strictly decodes a single JSON object: unknown fields, trailing
// data and oversized bodies are rejected.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return Errorf(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body exceeds %d bytes", MaxBodyBytes)
		}
		return Errorf(http.StatusBadRequest, "INVALID_JSON", "request body is not valid JSON for this endpoint")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Errorf(http.StatusBadRequest, "INVALID_JSON", "request body must contain a single JSON object")
	}
	return nil
}

// Recover turns a handler panic into a 500 and logs the stack.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.ErrorContext(r.Context(), "panic in HTTP handler", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
					WriteError(w, r, errors.New("panic"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// WithRequestID assigns a correlation ID. A client-supplied X-Request-Id is
// kept only if it is a UUID, so arbitrary text cannot be injected into logs.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if _, err := uuid.Parse(id); err != nil {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// AccessLog logs method, route pattern, status and duration. It logs the
// route pattern, not the raw path or query string, so identifiers and any
// personal data in URLs stay out of logs.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			log.InfoContext(r.Context(), "http request",
				"method", r.Method, "route", r.Pattern, "status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(), "request_id", RequestID(r.Context()))
		})
	}
}

// Authenticate verifies the bearer token and stores the principal. It does
// not decide what the principal may do.
func Authenticate(v *authn.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				WriteError(w, r, Errorf(http.StatusUnauthorized, "UNAUTHENTICATED", "a bearer token is required"))
				return
			}
			p, err := v.Verify(token)
			if err != nil {
				WriteError(w, r, Errorf(http.StatusUnauthorized, "UNAUTHENTICATED", "the access token is invalid or expired"))
				return
			}
			next.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), p)))
		})
	}
}

// RequireCustomer returns the authenticated customer or a 403 error.
func RequireCustomer(ctx context.Context) (authn.Principal, error) {
	p, ok := authn.PrincipalFrom(ctx)
	if !ok {
		return authn.Principal{}, Errorf(http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
	}
	if p.Kind != authn.KindCustomer {
		return authn.Principal{}, Errorf(http.StatusForbidden, "FORBIDDEN", "this endpoint is for customers")
	}
	return p, nil
}

// RequireStaff returns the authenticated staff member if they hold any of
// the given roles.
func RequireStaff(ctx context.Context, anyOf ...string) (authn.Principal, error) {
	p, ok := authn.PrincipalFrom(ctx)
	if !ok {
		return authn.Principal{}, Errorf(http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
	}
	if p.Kind != authn.KindStaff {
		return authn.Principal{}, Errorf(http.StatusForbidden, "FORBIDDEN", "this endpoint is for staff")
	}
	for _, role := range anyOf {
		if p.HasRole(role) {
			return p, nil
		}
	}
	return authn.Principal{}, Errorf(http.StatusForbidden, "FORBIDDEN", "your role does not permit this action")
}

// Chain applies middleware so that the first argument is outermost.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
