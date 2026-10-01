package simulator

import (
	"net/http"
	"sort"
	"time"
)

type reportItem struct {
	Reference   string `json:"reference"`
	ProviderRef string `json:"provider_ref"`
	AmountMinor int64  `json:"amount_minor"`
	Status      string `json:"status"`
	Settled     bool   `json:"settled"`
}

func (s *Server) settlementReport(w http.ResponseWriter, r *http.Request) {
	date, err := time.Parse(time.DateOnly, r.URL.Query().Get("date"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "BAD_DATE"})
		return
	}
	lagos := time.FixedZone("Africa/Lagos", 3600)
	s.mu.Lock()
	items := append([]reportItem{}, s.extraReport...)
	for _, t := range s.transfers {
		y, m, d := t.CreatedAt.In(lagos).Date()
		if s.hidden[t.Reference] || t.Status == StatusPending || !time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Equal(date) {
			continue
		}
		items = append(items, reportItem{Reference: t.Reference, ProviderRef: t.ProviderRef, AmountMinor: t.AmountMinor,
			Status: t.Status, Settled: t.Status == StatusSuccess})
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].Reference < items[j].Reference })
	writeJSON(w, http.StatusOK, map[string]any{"date": date.Format(time.DateOnly), "items": items})
}

// AddReportItem adds a line to every settlement report (for example a
// transfer the platform has no record of).
func (s *Server) AddReportItem(reference, providerRef string, amountMinor int64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extraReport = append(s.extraReport, reportItem{Reference: reference, ProviderRef: providerRef, AmountMinor: amountMinor, Status: status, Settled: status == StatusSuccess})
}

// HideFromReport omits a transfer from settlement reports.
func (s *Server) HideFromReport(reference string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hidden[reference] = true
}
