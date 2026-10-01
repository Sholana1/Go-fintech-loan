// Package httpapi is lending's REST surface for customers, staff and provider
// callbacks. Handlers authenticate, decode, call one use case, and encode.
// Business rules live in the app and domain packages, never here.
//
// Conventions (documented for clients in docs/contracts/lending-rest.md):
//   - Authentication: bearer access token issued by the identity service.
//   - Resource authorisation: a customer can only ever address their own
//     applications, offers and loans; another customer's id returns 404.
//     Staff endpoints require a role.
//   - Idempotency: every POST that creates something or moves money requires
//     an Idempotency-Key header. Repeating a request with the same key and
//     body returns the original resource; the same key with a different body
//     is rejected with 422 IDEMPOTENCY_KEY_REUSED.
//   - Errors: {"error":{"code","message","request_id"}} with stable codes.
//   - Money: integer minor units plus an ISO currency code.
//   - Versioning: the path carries the major version (/v1). Fields are only
//     added within a version.
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/services/lending/app"
)

// Config configures the handler.
type Config struct {
	Verifier *authn.Verifier
	// PayoutCallbacks and PaymentWebhooks authenticate provider
	// notifications. Nil disables the corresponding endpoint.
	PayoutCallbacks PayoutCallbackVerifier
	PaymentWebhooks PaymentWebhookVerifier
	// Guard rate-limits application submissions per customer. Nil disables
	// limiting. It is abuse protection only: "one open application per
	// customer" is enforced by a unique index in PostgreSQL.
	Guard            *ratelimit.Guard
	ApplicationLimit ratelimit.Limit
	Now              func() time.Time
	Logger           *slog.Logger
	// OnCallbackRejected is called when a callback fails authentication.
	OnCallbackRejected func()
}

type handler struct {
	svc *app.Service
	cfg Config
	log *slog.Logger
}

// New returns lending's HTTP handler.
func New(svc *app.Service, cfg Config) http.Handler {
	h := &handler{svc: svc, cfg: cfg, log: cfg.Logger}

	// One mux for every route, so the matched pattern (not the raw path,
	// which contains ids) is what the access log and traces record.
	root := http.NewServeMux()
	authenticate := httpx.Authenticate(cfg.Verifier)
	authed := func(pattern string, fn http.HandlerFunc) { root.Handle(pattern, authenticate(fn)) }

	// Customer endpoints.
	authed("POST /v1/loan-applications", h.submitApplication)
	authed("GET /v1/loan-applications/{id}", h.getApplication)
	authed("POST /v1/loan-applications/{id}/cancel", h.cancelApplication)
	authed("GET /v1/loan-offers/{id}", h.getOffer)
	authed("POST /v1/loan-offers/{id}/accept", h.acceptOffer)
	authed("GET /v1/loans", h.listLoans)
	authed("GET /v1/loans/{id}", h.getLoan)
	authed("GET /v1/loans/{id}/payoff-quote", h.payoffQuote)
	authed("GET /v1/loans/{id}/statement", h.statement)
	authed("POST /v1/loans/{id}/repayments", h.repay)
	authed("POST /v1/loans/{id}/external-repayments", h.initiateExternalRepayment)
	authed("GET /v1/loans/{id}/external-repayments/{payment_id}", h.getExternalRepayment)
	// Staff endpoints.
	authed("GET /v1/ops/manual-reviews", h.manualReviews)
	authed("POST /v1/ops/manual-reviews/{id}/decision", h.decideReview)
	authed("GET /v1/ops/admin-actions", h.listAdminActions)
	authed("POST /v1/ops/admin-actions", h.proposeAdminAction)
	authed("POST /v1/ops/admin-actions/{id}/approve", h.approveAdminAction)
	authed("POST /v1/ops/admin-actions/{id}/reject", h.rejectAdminAction)
	authed("GET /v1/ops/recon-exceptions", h.listExceptions)
	authed("POST /v1/ops/recon-exceptions/{id}/resolve", h.resolveException)
	authed("GET /v1/ops/portfolio", h.portfolio)
	authed("GET /v1/ops/loans/{id}", h.opsLoan)
	// Provider notifications are authenticated by signature, not by bearer
	// token.
	root.HandleFunc("POST /v1/provider-webhooks/payouts", h.payoutCallback)
	root.HandleFunc("POST /v1/provider-webhooks/payments", h.paymentWebhook)
	// Anything else: answer 401 without a token and 404 with one, so the
	// set of routes is not discoverable anonymously.
	root.Handle("/", authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, "NOT_FOUND", "the resource does not exist"))
	})))

	return httpx.Chain(root, httpx.Recover(cfg.Logger), httpx.WithRequestID, httpx.AccessLog(cfg.Logger))
}
