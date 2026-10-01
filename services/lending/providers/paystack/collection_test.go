package paystack_test

import (
	"net/http"
	"testing"

	"bankplatform.internal/services/lending/domain"
)

func TestVerifyPayment(t *testing.T) {
	const ref = "rp-5f0c2c9e-3a51-4f0e-9a6e-2f8f3f9d0b77"
	path := "GET /transaction/verify/" + ref
	tx := func(status string) map[string]any {
		return map[string]any{"id": 4099260516, "status": status, "reference": ref, "amount": 3_603_485, "currency": "NGN"}
	}

	t.Run("success carries what was collected", func(t *testing.T) {
		f := newFake(t)
		f.on(path, reply(http.StatusOK, true, "Verification successful", tx("success")))
		got, err := f.client().VerifyPayment(ctx, ref)
		if err != nil || got.Outcome != domain.ProviderSuccess || got.AmountMinor != 3_603_485 || got.Currency != "NGN" || got.ProviderRef != "4099260516" {
			t.Fatalf("verify: %+v %v", got, err)
		}
		if c := f.recorded()[0]; c.Auth != "Bearer "+testKey {
			t.Fatalf("Authorization %q", c.Auth)
		}
	})

	t.Run("statuses", func(t *testing.T) {
		for status, outcome := range map[string]domain.ProviderOutcome{
			"failed": domain.ProviderFailed, "reversed": domain.ProviderFailed,
			// The customer walked away from the checkout: not paid yet.
			"abandoned": domain.ProviderPending, "ongoing": domain.ProviderPending, "pending": domain.ProviderPending,
		} {
			f := newFake(t)
			f.on(path, reply(http.StatusOK, true, "", tx(status)))
			if got, err := f.client().VerifyPayment(ctx, ref); err != nil || got.Outcome != outcome {
				t.Errorf("status %q: %+v %v", status, got, err)
			}
		}
	})

	t.Run("no transaction yet is NOT_FOUND", func(t *testing.T) {
		f := newFake(t)
		f.on(path, reply(http.StatusNotFound, false, "Transaction reference not found", nil))
		if got, err := f.client().VerifyPayment(ctx, ref); err != nil || got.Outcome != domain.ProviderNotFound {
			t.Fatalf("verify: %+v %v", got, err)
		}
	})

	t.Run("anything unclear is unknown", func(t *testing.T) {
		for name, h := range map[string]http.HandlerFunc{
			"http 500":        reply(http.StatusInternalServerError, false, "", nil),
			"http 400":        reply(http.StatusBadRequest, false, "", nil),
			"unknown status":  reply(http.StatusOK, true, "", tx("teleported")),
			"other reference": reply(http.StatusOK, true, "", map[string]any{"status": "success", "reference": "rp-other", "amount": 1, "currency": "NGN"}),
			"not json":        func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("oops")) },
		} {
			f := newFake(t)
			f.on(path, h)
			if got, err := f.client().VerifyPayment(ctx, ref); err == nil {
				t.Errorf("%s: got %+v, want an error", name, got)
			}
		}
	})
}
