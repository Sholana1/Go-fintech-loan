package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/platform/httpx"
)

func (h *handler) listExceptions(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.RequireStaff(r.Context(), authn.RoleOpsMaker, authn.RoleOpsChecker, authn.RoleOpsViewer); err != nil {
		h.fail(w, r, err)
		return
	}
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "OPEN"
	}
	list, err := h.svc.ReconExceptions(r.Context(), state)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]exceptionJSON, 0, len(list))
	for _, e := range list {
		out = append(out, exceptionToJSON(e))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"exceptions": out})
}

func (h *handler) resolveException(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.RequireStaff(r.Context(), authn.RoleOpsChecker)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req noteRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.svc.ResolveReconException(r.Context(), uuid.MustParse(p.Subject), id, req.Note); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) portfolio(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.RequireStaff(r.Context(), authn.RoleOpsMaker, authn.RoleOpsChecker, authn.RoleOpsViewer); err != nil {
		h.fail(w, r, err)
		return
	}
	rows, err := h.svc.PortfolioReport(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"currency": h.svc.Product().Currency, "rows": rows})
}

func (h *handler) opsLoan(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.RequireStaff(r.Context(), authn.RoleOpsMaker, authn.RoleOpsChecker, authn.RoleOpsViewer, authn.RoleCreditReviewer); err != nil {
		h.fail(w, r, err)
		return
	}
	id, err := pathID(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.svc.OpsLoan(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, loanJSON(v))
}
