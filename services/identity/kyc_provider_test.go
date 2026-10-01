package identity_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bankplatform.internal/services/identity/app"
	"bankplatform.internal/services/identity/identitytest"
	"bankplatform.internal/services/identity/providers/bvnsim"
	"bankplatform.internal/services/ledger/ledgertest"
	sim "bankplatform.internal/simulator"
)

// The BVN adapter makes a real HTTP call. These tests run it against the
// provider simulator and against deliberately broken servers.
func TestBVNAdapter(t *testing.T) {
	const key = "kyc-key"
	provider := sim.New(sim.Options{APIKey: key})
	srv := httptest.NewServer(provider.Handler())
	t.Cleanup(srv.Close)
	dob := time.Date(1990, 5, 17, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("explicit answers", func(t *testing.T) {
		c := bvnsim.New(srv.URL, key, 2*time.Second)
		for bvn, want := range map[string]app.VerificationOutcome{
			"22200004242": app.OutcomeMatch, "22200001111": app.OutcomeMismatch, "22200000000": app.OutcomeNotFound,
		} {
			v, err := c.VerifyBVN(ctx, "ref-"+bvn, bvn, "Ada Obi", dob)
			if err != nil || v.Outcome != want || v.ProviderRef == "" || v.Provider != "bvn-simulator" {
				t.Errorf("bvn %s: %+v %v, want %s", bvn, v, err, want)
			}
		}
	})

	t.Run("the same reference is one enquiry at the provider", func(t *testing.T) {
		c := bvnsim.New(srv.URL, key, 2*time.Second)
		first, err := c.VerifyBVN(ctx, "ref-repeat", "22200004242", "Ada Obi", dob)
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.VerifyBVN(ctx, "ref-repeat", "22200004242", "Ada Obi", dob)
		if err != nil || second.ProviderRef != first.ProviderRef {
			t.Fatalf("repeat: %+v %v, want the verification %s again", second, err, first.ProviderRef)
		}
	})

	t.Run("anything but an explicit answer is an error", func(t *testing.T) {
		cases := map[string]struct {
			client *bvnsim.Client
			bvn    string
		}{
			"timeout":   {bvnsim.New(srv.URL, key, 200*time.Millisecond), "22200009999"},
			"malformed": {bvnsim.New(srv.URL, key, 2*time.Second), "22200008888"},
			"http 500":  {bvnsim.New(srv.URL, key, 2*time.Second), "22200007777"},
			"bad key":   {bvnsim.New(srv.URL, "wrong-key", 2*time.Second), "22200004242"},
			"down":      {bvnsim.New("http://127.0.0.1:1", key, 2*time.Second), "22200004242"},
		}
		for name, c := range cases {
			v, err := c.client.VerifyBVN(ctx, "ref-"+name, c.bvn, "Ada Obi", dob)
			if err == nil {
				t.Errorf("%s: got %+v, want an error", name, v)
				continue
			}
			if strings.Contains(err.Error(), c.bvn) {
				t.Errorf("%s: the BVN leaked into the error: %v", name, err)
			}
		}
	})

	t.Run("an unrecognised result is not treated as a match", func(t *testing.T) {
		odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"verification_ref":"KV-1","result":"PROBABLY"}`))
		}))
		t.Cleanup(odd.Close)
		if v, err := bvnsim.New(odd.URL, key, time.Second).VerifyBVN(ctx, "ref-odd", "22200004242", "Ada Obi", dob); err == nil {
			t.Fatalf("got %+v", v)
		}
	})
}

// Registration fails closed when the provider is slow, and the wait is
// bounded by the configured timeout.
func TestRegistrationWaitsNoLongerThanTheProviderTimeout(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{KYCTimeout: 300 * time.Millisecond})

	start := time.Now()
	st, body := env.Post(t, "/v1/customers", map[string]string{
		"phone": env.NextPhone(), "full_name": "Ada Test Customer", "date_of_birth": "1990-05-17", "bvn": env.NextBVN("9999"), "pin": "482915"})
	if st != http.StatusServiceUnavailable || errorCode(t, body) != "IDENTITY_PROVIDER_UNAVAILABLE" {
		t.Fatalf("slow provider: %d %s", st, body)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("registration waited %s for a provider with a 300ms timeout", took)
	}
	var n int
	if err := env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM identity.customers`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d customers created without a verified identity (%v)", n, err)
	}
}
