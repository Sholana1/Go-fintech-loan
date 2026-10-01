package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/app"
)

// PayoutCallbackVerifier authenticates a payout provider's callback from its
// headers and raw body and decodes it. Each provider adapter supplies one,
// because every provider signs differently. A callback that fails
// authentication is reported as app.ErrCallbackRejected.
type PayoutCallbackVerifier interface {
	VerifyPayoutCallback(h http.Header, body []byte, now time.Time) (app.PayoutCallback, error)
}

// PaymentWebhookVerifier does the same for inbound-payment webhooks.
type PaymentWebhookVerifier interface {
	VerifyPaymentWebhook(h http.Header, body []byte, now time.Time) (app.PaymentNotice, error)
}

// Provider notifications (payout callbacks, inbound-payment webhooks).
//
// They arrive from the internet with no bearer token, so the order of work
// matters:
//
//  1. read a bounded body (never an unbounded one from an unauthenticated
//     caller);
//  2. verify the signature over the RAW bytes, before any parsing;
//  3. only then decode;
//  4. hand it to the use case, which deduplicates on the provider's event id.
//
// Responses: 401 when authentication fails (nothing was done); 200 when the
// notification was handled, including when it was a duplicate, so the
// provider stops retrying; 5xx when we could not record it, so the provider
// retries.

// readNotification performs steps 1 to 3 and writes the error response
// itself when it returns false.
func (h *handler) readNotification(w http.ResponseWriter, r *http.Request, enabled bool) ([]byte, bool) {
	if !enabled {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusNotFound, "NOT_FOUND", "the resource does not exist"))
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, httpx.MaxBodyBytes))
	if err != nil {
		httpx.WriteError(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "callback body too large"))
		return nil, false
	}
	return body, true
}

// rejected answers a notification that failed authentication.
func (h *handler) rejected(w http.ResponseWriter, r *http.Request, kind string, err error) {
	if h.cfg.OnCallbackRejected != nil {
		h.cfg.OnCallbackRejected()
	}
	h.log.WarnContext(r.Context(), "provider notification rejected", "kind", kind, "reason", err.Error())
	httpx.WriteError(w, r, httpx.Errorf(http.StatusUnauthorized, "CALLBACK_REJECTED", "callback authentication failed"))
}

// payoutCallback receives the payout provider's notifications.
func (h *handler) payoutCallback(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readNotification(w, r, h.cfg.PayoutCallbacks != nil)
	if !ok {
		return
	}
	cb, err := h.cfg.PayoutCallbacks.VerifyPayoutCallback(r.Header, body, h.cfg.Now())
	switch {
	case errors.Is(err, app.ErrCallbackRejected):
		h.rejected(w, r, "payout", err)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	if err := h.svc.HandlePayoutCallback(r.Context(), cb); err != nil {
		// A 5xx makes the provider retry, which is what we want if we could
		// not record the outcome.
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// paymentWebhook receives the payment provider's notifications about money
// customers paid in. The webhook is a hint: the use case asks the provider
// what really happened before anything is credited.
func (h *handler) paymentWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readNotification(w, r, h.cfg.PaymentWebhooks != nil)
	if !ok {
		return
	}
	notice, err := h.cfg.PaymentWebhooks.VerifyPaymentWebhook(r.Header, body, h.cfg.Now())
	switch {
	case errors.Is(err, app.ErrCallbackRejected):
		h.rejected(w, r, "payment", err)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	if err := h.svc.HandlePaymentNotice(r.Context(), notice); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
