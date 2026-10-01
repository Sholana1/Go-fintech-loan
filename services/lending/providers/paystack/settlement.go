package paystack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

const (
	transfersPerPage = 100
	// maxTransferPages bounds one report. At 100 per page this is 50,000
	// transfers for one business date; beyond that the report is refused
	// instead of being silently truncated.
	maxTransferPages = 500
)

// SettlementReport lists the transfers Paystack recorded for a business
// date: GET /transfer?from=&to=&per_page=&page=.
//
// Paystack sends transfers from our prefunded balance, so a transfer with
// status "success" has left that balance: it is reported as settled.
//
// The specification does not say which timestamp from/to filter on. The
// window used is the business date in Lagos time. Reconciliation matches
// lines by reference, so a transfer falling on the other side of midnight
// is matched when the neighbouring date is reconciled (the caller
// reconciles today and yesterday).
func (c *Client) SettlementReport(ctx context.Context, businessDate time.Time) ([]app.SettlementItem, error) {
	from := bizdate.StartOf(businessDate)
	to := from.AddDate(0, 0, 1).Add(-time.Second)

	var items []app.SettlementItem
	for page := 1; ; page++ {
		if page > maxTransferPages {
			return nil, errors.New("paystack list transfers: more pages than this adapter will read; the report is incomplete")
		}
		r, err := c.call(ctx, http.MethodGet, "/transfer", url.Values{
			"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)},
			"per_page": {strconv.Itoa(transfersPerPage)}, "page": {strconv.Itoa(page)},
		}, nil)
		if err != nil {
			return nil, err
		}
		if r.HTTPStatus != http.StatusOK || !r.OK {
			return nil, r.unexpected("list transfers")
		}
		var rows []transferData
		if err := json.Unmarshal(r.Data, &rows); err != nil {
			return nil, fmt.Errorf("paystack list transfers: response is malformed: %w", err)
		}
		var meta struct {
			PageCount int `json:"pageCount"`
		}
		if err := json.Unmarshal(r.Meta, &meta); err != nil {
			return nil, fmt.Errorf("paystack list transfers: pagination is malformed: %w", err)
		}
		for _, t := range rows {
			outcome, _, err := transferOutcome(t.Status)
			if err != nil {
				return nil, fmt.Errorf("paystack list transfers: %s: %w", t.Reference, err)
			}
			if outcome == domain.ProviderPending {
				continue // not final: it does not belong in a settlement report
			}
			items = append(items, app.SettlementItem{
				Reference: t.Reference, ProviderRef: t.TransferCode, AmountMinor: t.Amount,
				Outcome: outcome, Settled: outcome == domain.ProviderSuccess,
			})
		}
		if page >= meta.PageCount || len(rows) == 0 {
			return items, nil
		}
	}
}

var _ app.PayoutProvider = (*Client)(nil)
