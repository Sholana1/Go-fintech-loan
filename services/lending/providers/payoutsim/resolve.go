package payoutsim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"bankplatform.internal/services/lending/app"
)

func (c *Client) ResolveAccount(ctx context.Context, bankCode, accountNumber string) (app.ResolvedAccount, error) {
	status, raw, err := c.do(ctx, http.MethodPost, "/payout/v1/accounts/resolve", map[string]string{"bank_code": bankCode, "account_number": accountNumber})
	if err != nil {
		return app.ResolvedAccount{}, fmt.Errorf("name enquiry: %w", err)
	}
	if status == http.StatusNotFound {
		return app.ResolvedAccount{}, app.ErrAccountNotResolved
	}
	if status != http.StatusOK {
		return app.ResolvedAccount{}, fmt.Errorf("name enquiry returned HTTP %d", status)
	}
	var out struct {
		AccountName string `json:"account_name"`
		EnquiryRef  string `json:"enquiry_ref"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccountName == "" || out.EnquiryRef == "" {
		return app.ResolvedAccount{}, errors.New("name enquiry response is malformed")
	}
	return app.ResolvedAccount{AccountName: out.AccountName, EnquiryRef: out.EnquiryRef}, nil
}
