package paystack

import (
	"context"
	"net/http"
	"net/url"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

// Query asks Paystack what happened to a transfer:
// GET /transfer/verify/{reference}.
//
// This is how an unknown outcome is resolved. It is a read, so the caller
// repeats it on a backoff schedule until the answer is final.
//
// NOT_FOUND (HTTP 404) means Paystack has no transfer under the reference.
// It is not proof that none will appear: a request still in flight could
// yet be recorded. The caller keeps the funds held and asks again.
func (c *Client) Query(ctx context.Context, reference string) (app.PayoutResult, error) {
	r, err := c.call(ctx, http.MethodGet, "/transfer/verify/"+url.PathEscape(reference), nil, nil)
	if err != nil {
		return app.PayoutResult{}, err
	}
	switch {
	case r.HTTPStatus == http.StatusNotFound:
		return app.PayoutResult{Outcome: domain.ProviderNotFound, Code: "NOT_FOUND"}, nil
	case r.HTTPStatus != http.StatusOK || !r.OK:
		return app.PayoutResult{}, r.unexpected("verify transfer")
	}
	// The amount is not re-checked here (0): reconciliation compares amounts
	// against the transfer list and raises a mismatch exception.
	return c.result(r.Data, reference, 0)
}
