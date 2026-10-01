package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/platform/httpx"
	"bankplatform.internal/services/lending/domain"
)

type proposeRequest struct {
	Kind   string         `json:"kind"`
	LoanID string         `json:"loan_id"`
	Params map[string]any `json:"params"`
	Reason string         `json:"reason"`
}

func (h *handler) proposeAdminAction(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.RequireStaff(r.Context(), authn.RoleOpsMaker)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req proposeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	loanID, err := uuid.Parse(req.LoanID)
	if err != nil {
		h.fail(w, r, httpx.Errorf(http.StatusBadRequest, "VALIDATION_FAILED", "loan_id must be a UUID"))
		return
	}
	params, err := marshalParams(req.Params)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	a, err := h.svc.ProposeAdminAction(r.Context(), uuid.MustParse(p.Subject), domain.AdminActionKind(req.Kind), loanID, params, req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, adminActionJSON(a))
}

type noteRequest struct {
	Note string `json:"note"`
}

func (h *handler) approveAdminAction(w http.ResponseWriter, r *http.Request) {
	h.decideAction(w, r, true)
}

func (h *handler) rejectAdminAction(w http.ResponseWriter, r *http.Request) {
	h.decideAction(w, r, false)
}

func (h *handler) decideAction(w http.ResponseWriter, r *http.Request, approve bool) {
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
	var a domain.AdminAction
	if approve {
		a, err = h.svc.ApproveAdminAction(r.Context(), uuid.MustParse(p.Subject), id, req.Note)
	} else {
		a, err = h.svc.RejectAdminAction(r.Context(), uuid.MustParse(p.Subject), id, req.Note)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, adminActionJSON(a))
}

func (h *handler) listAdminActions(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.RequireStaff(r.Context(), authn.RoleOpsMaker, authn.RoleOpsChecker, authn.RoleOpsViewer); err != nil {
		h.fail(w, r, err)
		return
	}
	actions, err := h.svc.OpenAdminActions(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]adminAction, 0, len(actions))
	for _, a := range actions {
		out = append(out, adminActionJSON(a))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"actions": out})
}
