package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

func (h *handler) getOffer(w http.ResponseWriter, r *http.Request) {
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
	o, err := h.svc.GetOffer(r.Context(), uuid.MustParse(p.Subject), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, offerJSON(o))
}

func (h *handler) acceptOffer(w http.ResponseWriter, r *http.Request) {
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
	var req app.AcceptRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	loan, replay, err := h.svc.AcceptOffer(r.Context(), app.Acceptor{CustomerID: uuid.MustParse(p.Subject), TokenID: p.TokenID}, id, idempotencyKey(r), req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	view, err := h.svc.GetLoan(r.Context(), loan.CustomerID, loan.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// 201 when the money is in the customer's account; 202 when the loan is
	// accepted and the disbursement is still being posted. The body's status
	// field says which, and never says "disbursed" before the ledger has.
	status := http.StatusCreated
	if loan.State == domain.LoanPendingDisbursement {
		status = http.StatusAccepted
	}
	httpx.WriteJSON(w, status, loanJSON(view))
}
