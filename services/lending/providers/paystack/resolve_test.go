package paystack_test

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"bankplatform.internal/services/lending/app"
)

func TestResolveAccount(t *testing.T) {
	t.Run("returns the account name", func(t *testing.T) {
		f := newFake(t)
		f.on("GET /bank/resolve", reply(http.StatusOK, true, "Account number resolved",
			map[string]any{"account_number": "0123456789", "account_name": "ADA OBI", "bank_id": 9}))
		got, err := f.client().ResolveAccount(ctx, "058", "0123456789")
		if err != nil || got.AccountName != "ADA OBI" || got.EnquiryRef == "" {
			t.Fatalf("resolve: %+v %v", got, err)
		}
		q, _ := url.ParseQuery(f.recorded()[0].Query)
		// The bank code keeps its leading zero.
		if q.Get("account_number") != "0123456789" || q.Get("bank_code") != "058" {
			t.Fatalf("query: %v", q)
		}
	})

	t.Run("a declined enquiry means the account could not be resolved", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusBadRequest} {
			f := newFake(t)
			f.on("GET /bank/resolve", reply(status, false, "Could not resolve account name", nil))
			if _, err := f.client().ResolveAccount(ctx, "058", "0000000000"); !errors.Is(err, app.ErrAccountNotResolved) {
				t.Errorf("HTTP %d: %v, want ErrAccountNotResolved", status, err)
			}
		}
	})

	t.Run("an outage is an error, not a verdict on the account", func(t *testing.T) {
		for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusTooManyRequests} {
			f := newFake(t)
			f.on("GET /bank/resolve", reply(status, false, "", nil))
			if _, err := f.client().ResolveAccount(ctx, "058", "0123456789"); err == nil || errors.Is(err, app.ErrAccountNotResolved) {
				t.Errorf("HTTP %d: %v, want a plain error", status, err)
			}
		}
	})

	t.Run("an answer about a different account is refused", func(t *testing.T) {
		f := newFake(t)
		f.on("GET /bank/resolve", reply(http.StatusOK, true, "", map[string]any{"account_number": "9999999999", "account_name": "SOMEONE ELSE", "bank_id": 9}))
		if got, err := f.client().ResolveAccount(ctx, "058", "0123456789"); err == nil {
			t.Fatalf("accepted %+v", got)
		}
	})
}
