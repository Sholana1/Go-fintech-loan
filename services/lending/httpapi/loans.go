package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/httpx"
)

func (h *handler) listLoans(w http.ResponseWriter, r *http.Request) {
	p, err := httpx.RequireCustomer(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	loans, err := h.svc.ListLoans(r.Context(), uuid.MustParse(p.Subject))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]loanSummary, 0, len(loans))
	for _, l := range loans {
		out = append(out, summariseLoan(l))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"loans": out})
}

func (h *handler) getLoan(w http.ResponseWriter, r *http.Request) {
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
	v, err := h.svc.GetLoan(r.Context(), uuid.MustParse(p.Subject), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, loanJSON(v))
}

func (h *handler) payoffQuote(w http.ResponseWriter, r *http.Request) {
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
	q, asOf, err := h.svc.PayoffQuote(r.Context(), uuid.MustParse(p.Subject), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, payoffJSON{
		AsOf: asOf.Format(time.DateOnly), Currency: string(h.svc.Product().Currency),
		PrincipalMinor: q.Principal, InterestMinor: q.Interest, FeesMinor: q.Fees, TotalMinor: q.Total(),
	})
}

func (h *handler) statement(w http.ResponseWriter, r *http.Request) {
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
	st, err := h.svc.GetStatement(r.Context(), uuid.MustParse(p.Subject), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, statementJSON(st, string(h.svc.Product().Currency)))
}
