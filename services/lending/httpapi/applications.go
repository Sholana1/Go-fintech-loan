package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/app"
)

func (h *handler) submitApplication(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.RequireCustomer(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Counted per customer. A burst of submissions is either a client bug or
	// abuse of the (paid) credit-bureau enquiry behind each application.
	if !h.cfg.Guard.Allow(w, r, h.cfg.ApplicationLimit, p.Subject) {
		return
	}
	var req app.SubmitRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	a, replay, err := h.svc.SubmitApplication(r.Context(), uuid.MustParse(p.Subject), idempotencyKey(r), req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusAccepted // assessment continues asynchronously
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	httpx.WriteJSON(w, status, applicationJSON(app.ApplicationView{Application: a}))
}

func (h *handler) getApplication(w http.ResponseWriter, r *http.Request) {
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
	v, err := h.svc.GetApplication(r.Context(), uuid.MustParse(p.Subject), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, applicationJSON(v))
}

func (h *handler) cancelApplication(w http.ResponseWriter, r *http.Request) {
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
	customer := uuid.MustParse(p.Subject)
	if err := h.svc.CancelApplication(r.Context(), customer, id); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.svc.GetApplication(r.Context(), customer, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, applicationJSON(v))
}
