package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/app"
)

func (h *handler) manualReviews(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.RequireStaff(r.Context(), authn.RoleCreditReviewer); err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.svc.ManualReviewQueue(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]reviewItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, reviewJSON(it))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handler) decideReview(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.RequireStaff(r.Context(), authn.RoleCreditReviewer)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req app.ReviewDecision
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.svc.DecideManualReview(r.Context(), uuid.MustParse(p.Subject), id, req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, applicationJSON(v))
}
