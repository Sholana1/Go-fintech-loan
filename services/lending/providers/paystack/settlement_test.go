package paystack_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"bankplatform.internal/services/lending/domain"
)

func TestSettlementReportReadsEveryPage(t *testing.T) {
	f := newFake(t)
	pages := map[string][]map[string]any{
		"1": {
			{"reference": "lp-a", "status": "success", "transfer_code": "TRF_a", "amount": 100, "currency": "NGN"},
			{"reference": "lp-b", "status": "failed", "transfer_code": "TRF_b", "amount": 200, "currency": "NGN"},
		},
		"2": {
			{"reference": "lp-c", "status": "pending", "transfer_code": "TRF_c", "amount": 300, "currency": "NGN"},
			{"reference": "lp-d", "status": "reversed", "transfer_code": "TRF_d", "amount": 400, "currency": "NGN"},
		},
	}
	f.on("GET /transfer", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// The business date 2026-10-01 in Lagos (UTC+1).
		if q.Get("from") != "2026-10-01T00:00:00+01:00" || q.Get("to") != "2026-10-01T23:59:59+01:00" || q.Get("per_page") != "100" {
			t.Errorf("list query: %v", q)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "message": "Transfers retrieved",
			"data": pages[q.Get("page")], "meta": map[string]any{"total": 4, "skipped": 0, "perPage": 100, "page": 1, "pageCount": 2}})
	})

	items, err := f.client().SettlementReport(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	// The pending transfer is not final and is left out.
	if len(items) != 3 {
		t.Fatalf("%d items: %+v", len(items), items)
	}
	if a := items[0]; a.Reference != "lp-a" || a.Outcome != domain.ProviderSuccess || !a.Settled || a.AmountMinor != 100 || a.ProviderRef != "TRF_a" {
		t.Fatalf("first item: %+v", a)
	}
	if b, d := items[1], items[2]; b.Outcome != domain.ProviderFailed || b.Settled || d.Reference != "lp-d" || d.Outcome != domain.ProviderFailed {
		t.Fatalf("failed items: %+v %+v", b, d)
	}
	if n := len(f.recorded()); n != 2 {
		t.Fatalf("%d page requests, want 2", n)
	}
}

// A report that cannot be read completely is an error, never a partial list:
// reconciliation must not conclude anything from half a report.
func TestSettlementReportIsAllOrNothing(t *testing.T) {
	f := newFake(t)
	f.on("GET /transfer", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"status":false,"message":"bad gateway"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": true, "message": "",
			"data": []map[string]any{{"reference": "lp-a", "status": "success", "amount": 100}},
			"meta": map[string]any{"pageCount": 2}})
	})
	if items, err := f.client().SettlementReport(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatalf("got a partial report: %+v", items)
	}
}
