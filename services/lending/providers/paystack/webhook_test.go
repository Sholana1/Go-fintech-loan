package paystack_test

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
	"time"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/providers/paystack"
)

func sign(key string, body []byte) string {
	m := hmac.New(sha512.New, []byte(key))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func TestWebhookAuthentication(t *testing.T) {
	hooks := paystack.Webhooks{SecretKey: testKey}
	body := []byte(`{"event":"transfer.success","data":{"reference":"lp-abc","status":"success","amount":9900000}}`)
	header := func(sig string) http.Header { return http.Header{"X-Paystack-Signature": []string{sig}} }

	t.Run("a valid signature yields only a reference, never an outcome", func(t *testing.T) {
		cb, err := hooks.VerifyPayoutCallback(header(sign(testKey, body)), body, time.Now())
		if err != nil || cb.Reference != "lp-abc" || cb.EventID == "" || cb.Provider != "paystack" {
			t.Fatalf("callback: %+v %v", cb, err)
		}
		if cb.Outcome != "" {
			t.Fatalf("the webhook body was trusted for an outcome: %q", cb.Outcome)
		}
		// The same delivery has the same id, so a redelivery is recognised.
		again, _ := hooks.VerifyPayoutCallback(header(sign(testKey, body)), body, time.Now())
		if again.EventID != cb.EventID {
			t.Fatal("event id is not stable across redeliveries")
		}
		notice, err := hooks.VerifyPaymentWebhook(header(sign(testKey, body)), body, time.Now())
		if err != nil || notice.Reference != "lp-abc" {
			t.Fatalf("payment webhook: %+v %v", notice, err)
		}
	})

	t.Run("bad signatures are rejected", func(t *testing.T) {
		tampered := []byte(`{"event":"transfer.success","data":{"reference":"lp-xyz"}}`)
		for name, c := range map[string]struct {
			sig  string
			body []byte
		}{
			"missing":        {"", body},
			"not hex":        {"zz", body},
			"wrong key":      {sign("sk_test_other", body), body},
			"tampered body":  {sign(testKey, body), tampered},
			"sha256 instead": {"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", body},
		} {
			if _, err := hooks.VerifyPayoutCallback(header(c.sig), c.body, time.Now()); !errors.Is(err, app.ErrCallbackRejected) {
				t.Errorf("%s: %v, want ErrCallbackRejected", name, err)
			}
		}
	})

	t.Run("a signed body without a reference is invalid, not rejected", func(t *testing.T) {
		empty := []byte(`{"event":"charge.success","data":{}}`)
		_, err := hooks.VerifyPaymentWebhook(header(sign(testKey, empty)), empty, time.Now())
		if err == nil || errors.Is(err, app.ErrCallbackRejected) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestConfigurationIsValidated(t *testing.T) {
	if _, err := paystack.New(paystack.Config{Currency: "NGN"}); err == nil {
		t.Fatal("accepted a missing secret key")
	}
	if _, err := paystack.New(paystack.Config{SecretKey: testKey}); err == nil {
		t.Fatal("accepted a missing currency")
	}
	// The secret key must never travel over plain HTTP to a remote host.
	if _, err := paystack.New(paystack.Config{BaseURL: "http://api.paystack.co", SecretKey: testKey, Currency: "NGN"}); err == nil {
		t.Fatal("accepted a plain-HTTP remote base URL")
	}
	c, err := paystack.New(paystack.Config{SecretKey: testKey, Currency: "NGN"})
	if err != nil || c.Name() != "paystack" {
		t.Fatalf("default configuration: %v", err)
	}
}
