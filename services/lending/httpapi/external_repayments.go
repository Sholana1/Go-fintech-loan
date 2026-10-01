package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/domain"
)

type externalRepaymentRequest struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type externalRepaymentResponse struct {
	PaymentID string `json:"payment_id"`
	LoanID    string `json:"loan_id"`
	// Reference is what the customer pays with at the provider.
	Reference     string     `json:"reference"`
	Provider      string     `json:"provider"`
	Status        string     `json:"status"`
	ExpectedMinor int64      `json:"expected_minor"`
	ReceivedMinor *int64     `json:"received_minor,omitempty"`
	Currency      string     `json:"currency"`
	RepaymentID   *uuid.UUID `json:"repayment_id,omitempty"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

// externalPaymentStatus maps internal states to what a customer needs to
// know. Internal bookkeeping steps are all "PROCESSING".
func externalPaymentStatus(s domain.ExternalPaymentState) string {
	switch s {
	case domain.ExternalInitiated:
		return "AWAITING_PAYMENT"
	case domain.ExternalVerified, domain.ExternalCredited, domain.ExternalReview:
		return "PROCESSING"
	case domain.ExternalApplied:
		return "APPLIED"
	case domain.ExternalUnapplied:
		return "CREDITED_TO_ACCOUNT"
	case domain.ExternalFailed:
		return "FAILED"
	case domain.ExternalExpired:
		return "EXPIRED"
	default:
		return "PROCESSING"
	}
}

func externalRepaymentJSON(p domain.ExternalPayment) externalRepaymentResponse {
	return externalRepaymentResponse{
		PaymentID: p.ID.String(), LoanID: p.LoanID.String(), Reference: p.Reference, Provider: p.Provider,
		Status: externalPaymentStatus(p.State), ExpectedMinor: p.ExpectedMinor, ReceivedMinor: p.VerifiedMinor,
		Currency: p.Currency, RepaymentID: p.RepaymentID, ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt,
	}
}

// initiateExternalRepayment issues a payment reference. It moves no money.
func (h *handler) initiateExternalRepayment(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.RequireCustomer(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.svc.ExternalRepaymentsEnabled() {
		h.fail(w, r, httpx.Errorf(http.StatusNotFound, "NOT_FOUND", "the resource does not exist"))
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req externalRepaymentRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.Currency != string(h.svc.Product().Currency) {
		h.fail(w, r, httpx.Errorf(http.StatusBadRequest, "VALIDATION_FAILED", "currency must be %s", h.svc.Product().Currency))
		return
	}
	payment, replay, err := h.svc.InitiateExternalRepayment(r.Context(), uuid.MustParse(p.Subject), id, idempotencyKey(r), req.AmountMinor)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	httpx.WriteJSON(w, http.StatusCreated, externalRepaymentJSON(payment))
}

func (h *handler) getExternalRepayment(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.RequireCustomer(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	loanID, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	paymentID, err := uuid.Parse(r.PathValue("payment_id"))
	if err != nil {
		h.fail(w, r, httpx.Errorf(http.StatusNotFound, "NOT_FOUND", "the resource does not exist"))
		return
	}
	payment, err := h.svc.GetExternalRepayment(r.Context(), uuid.MustParse(p.Subject), loanID, paymentID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, externalRepaymentJSON(payment))
}
