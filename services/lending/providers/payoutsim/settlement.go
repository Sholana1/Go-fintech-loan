package payoutsim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"bankplatform.internal/services/lending/app"
)

func (c *Client) SettlementReport(ctx context.Context, businessDate time.Time) ([]app.SettlementItem, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/payout/v1/settlement-report?date="+businessDate.Format(time.DateOnly), nil)
	if err != nil {
		return nil, fmt.Errorf("settlement report: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("settlement report returned HTTP %d", status)
	}
	var out struct {
		Items []struct {
			Reference   string `json:"reference"`
			ProviderRef string `json:"provider_ref"`
			AmountMinor int64  `json:"amount_minor"`
			Status      string `json:"status"`
			Settled     bool   `json:"settled"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("settlement report is malformed: %w", err)
	}
	items := make([]app.SettlementItem, 0, len(out.Items))
	for _, it := range out.Items {
		o, err := outcome(it.Status)
		if err != nil {
			return nil, fmt.Errorf("settlement report line %q: %w", it.Reference, err)
		}
		items = append(items, app.SettlementItem{Reference: it.Reference, ProviderRef: it.ProviderRef, AmountMinor: it.AmountMinor, Outcome: o, Settled: it.Settled})
	}
	return items, nil
}
