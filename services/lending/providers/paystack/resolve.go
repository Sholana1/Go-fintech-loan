package paystack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"bankplatform.internal/services/lending/app"
)

// ResolveAccount is name enquiry: GET /bank/resolve?account_number=&bank_code=
//
// It is a read. Nothing is at stake if it is wrong in the cautious
// direction, so a request Paystack declines (4xx) is reported as "could not
// be resolved" and anything else unexpected as an error (try again).
func (c *Client) ResolveAccount(ctx context.Context, bankCode, accountNumber string) (app.ResolvedAccount, error) {
	r, err := c.call(ctx, http.MethodGet, "/bank/resolve", url.Values{"account_number": {accountNumber}, "bank_code": {bankCode}}, nil)
	if err != nil {
		return app.ResolvedAccount{}, err
	}
	if r.refused() {
		return app.ResolvedAccount{}, app.ErrAccountNotResolved
	}
	if r.HTTPStatus != http.StatusOK || !r.OK {
		return app.ResolvedAccount{}, r.unexpected("resolve account")
	}
	var data struct {
		AccountNumber string `json:"account_number"`
		AccountName   string `json:"account_name"`
		BankID        int64  `json:"bank_id"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil || data.AccountName == "" {
		return app.ResolvedAccount{}, errors.New("paystack resolve account: response is malformed")
	}
	if data.AccountNumber != "" && data.AccountNumber != accountNumber {
		return app.ResolvedAccount{}, errors.New("paystack resolve account: response is for a different account")
	}
	// Paystack returns no enquiry reference; the bank id is the only
	// provider-side identifier in the response.
	return app.ResolvedAccount{AccountName: data.AccountName, EnquiryRef: "paystack-bank-" + strconv.FormatInt(data.BankID, 10)}, nil
}
