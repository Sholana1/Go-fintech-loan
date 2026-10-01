package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/domain"
)

type repayRequest struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func (h *handler) repay(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.RequireCustomer(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req repayRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if req.Currency != string(h.svc.Product().Currency) {
		h.fail(w, r, httpx.Errorf(http.StatusBadRequest, "VALIDATION_FAILED", "currency must be %s", h.svc.Product().Currency))
		return
	}
	rep, replay, err := h.svc.Repay(r.Context(), uuid.MustParse(p.Subject), id, idempotencyKey(r), req.AmountMinor, domain.SourceCustomer)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	switch rep.State {
	case domain.RepaymentAllocated:
		httpx.WriteJSON(w, http.StatusCreated, repaymentJSON(rep))
	case domain.RepaymentRejected:
		code := "REPAYMENT_REJECTED"
		if rep.RejectReason == "INSUFFICIENT_FUNDS" {
			code = "INSUFFICIENT_FUNDS"
		}
		h.fail(w, r, httpx.Errorf(http.StatusUnprocessableEntity, code, "the repayment could not be taken from your account"))
	default:
		// The posting is durable and will complete; the customer sees
		// "processing", not "failed".
		httpx.WriteJSON(w, http.StatusAccepted, repaymentJSON(rep))
	}
}
