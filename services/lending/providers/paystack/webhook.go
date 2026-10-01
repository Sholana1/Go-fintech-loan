package paystack

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

// Webhooks authenticates Paystack webhooks.
//
// STATUS: UNVERIFIED. Webhooks are not described in the OpenAPI
// specification this adapter was written from. The signing scheme below is
// Paystack's published one as recalled (the documentation page could not be
// fetched when this was written): header "x-paystack-signature" carries the
// hex HMAC-SHA512 of the raw request body, keyed with the integration's
// secret key. It must be confirmed against the sandbox before being
// enabled, which is why enabling it is an explicit configuration switch.
//
// The design limits what an error in that recollection could cost:
//
//   - Only one field of the body is used, data.reference. Neither the event
//     name nor any status or amount in the body is trusted.
//   - A webhook therefore never states an outcome. It makes the service ask
//     Paystack (Query or VerifyPayment) about that reference, and the
//     answer to that authenticated call is what is applied.
//   - If webhooks are disabled, wrong, or lost, the status-query schedule
//     reaches the same result, only later.
//
// The scheme has no timestamp, so a captured webhook can be replayed
// forever. Because a webhook only triggers a query, a replay costs one
// read against Paystack and changes nothing.
type Webhooks struct {
	SecretKey string
}

func (v Webhooks) reference(h http.Header, body []byte) (reference, eventID string, err error) {
	mac := hmac.New(sha512.New, []byte(v.SecretKey))
	mac.Write(body)
	got, decodeErr := hex.DecodeString(h.Get("x-paystack-signature"))
	if decodeErr != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return "", "", fmt.Errorf("%w: signature is invalid", app.ErrCallbackRejected)
	}
	var event struct {
		Data struct {
			Reference string `json:"reference"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &event); err != nil || event.Data.Reference == "" {
		return "", "", fmt.Errorf("%w: webhook carries no reference", domain.ErrValidation)
	}
	// Paystack's event carries no delivery id we could verify, so the
	// digest of the body identifies the delivery: a redelivery of the same
	// event has the same body.
	sum := sha256.Sum256(body)
	return event.Data.Reference, hex.EncodeToString(sum[:]), nil
}

// VerifyPayoutCallback authenticates a transfer webhook. The outcome is left
// empty on purpose: the service queries Paystack for it.
func (v Webhooks) VerifyPayoutCallback(h http.Header, body []byte, _ time.Time) (app.PayoutCallback, error) {
	reference, eventID, err := v.reference(h, body)
	if err != nil {
		return app.PayoutCallback{}, err
	}
	return app.PayoutCallback{Provider: ProviderName, EventID: eventID, Reference: reference, Raw: body}, nil
}

// VerifyPaymentWebhook authenticates a payment webhook.
func (v Webhooks) VerifyPaymentWebhook(h http.Header, body []byte, _ time.Time) (app.PaymentNotice, error) {
	reference, eventID, err := v.reference(h, body)
	if err != nil {
		return app.PaymentNotice{}, err
	}
	return app.PaymentNotice{Provider: ProviderName, EventID: eventID, Reference: reference, Raw: body}, nil
}
