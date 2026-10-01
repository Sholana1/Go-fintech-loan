package paystack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

// VerifyPayment asks Paystack about a payment a customer made to us:
// GET /transaction/verify/{reference}.
//
// This call, and nothing else, decides whether a customer is credited. The
// customer's app saying "paid" does not, and a webhook does not. A SUCCESS
// answer carries the amount and currency Paystack actually collected, and
// those are what get credited.
func (c *Client) VerifyPayment(ctx context.Context, reference string) (app.PaymentStatus, error) {
	r, err := c.call(ctx, http.MethodGet, "/transaction/verify/"+url.PathEscape(reference), nil, nil)
	if err != nil {
		return app.PaymentStatus{}, err
	}
	switch {
	case r.HTTPStatus == http.StatusNotFound:
		// No transaction under the reference: the customer has not started
		// paying. Normal before payment; the caller asks again later.
		return app.PaymentStatus{Outcome: domain.ProviderNotFound, Code: "NOT_FOUND"}, nil
	case r.HTTPStatus != http.StatusOK || !r.OK:
		return app.PaymentStatus{}, r.unexpected("verify transaction")
	}
	var data struct {
		ID        int64  `json:"id"`
		Status    string `json:"status"`
		Reference string `json:"reference"`
		Amount    int64  `json:"amount"`
		Currency  string `json:"currency"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		return app.PaymentStatus{}, fmt.Errorf("paystack verify transaction: response is malformed: %w", err)
	}
	if data.Reference != reference {
		return app.PaymentStatus{}, fmt.Errorf("paystack verify transaction: response is for reference %q, expected %q", data.Reference, reference)
	}
	outcome, code, err := paymentOutcome(data.Status)
	if err != nil {
		return app.PaymentStatus{}, err
	}
	return app.PaymentStatus{
		Outcome: outcome, AmountMinor: data.Amount, Currency: data.Currency,
		ProviderRef: strconv.FormatInt(data.ID, 10), Code: code,
	}, nil
}

var _ app.PaymentVerifier = (*Client)(nil)
