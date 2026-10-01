// Package bureausim is the client adapter for the SIMULATED credit bureau
// (see package sim). It implements app.CreditBureau over HTTP.
//
// Status: UNVERIFIED against any real bureau. A production adapter needs the
// bureau's API documentation, sandbox credentials, and a data-sharing
// agreement; none of those exist yet (see docs/loans/LAUNCH_DEPENDENCIES.md).
// What this adapter does establish, and what a production adapter must keep:
//
//   - every call has a deadline and uses our request reference so a retry is
//     the same enquiry;
//   - the response is decoded strictly and validated; a response that is
//     malformed, incomplete or out of range is an ERROR, never a clean file;
//   - the raw response is returned unmodified for the decision record.
package bureausim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

// ProviderName identifies this adapter in decision records and metrics.
const ProviderName = "bureau-simulator"

// Client implements app.CreditBureau.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	now     func() time.Time
}

// New returns a client. The HTTP client's own timeout is a backstop; the
// per-call deadline comes from the context.
func New(baseURL, apiKey string, now func() time.Time) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, now: now, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) Name() string { return ProviderName }

type response struct {
	ReportRef               string `json:"report_ref"`
	Score                   *int   `json:"score"`
	ActiveLoans             *int   `json:"active_loans"`
	TotalOutstandingMinor   *int64 `json:"total_outstanding_minor"`
	MonthlyObligationsMinor *int64 `json:"monthly_obligations_minor"`
	WorstDelinquencyDays12m *int   `json:"worst_delinquency_days_12m"`
	AsOf                    string `json:"as_of"`
}

// ErrMalformed marks a response that could not be trusted.
var ErrMalformed = errors.New("credit bureau response is malformed")

func (c *Client) FetchReport(ctx context.Context, requestRef string, subject app.BureauSubject) (domain.BureauReport, error) {
	body, err := json.Marshal(map[string]string{
		"request_ref": requestRef, "bvn": subject.BVN, "full_name": subject.FullName,
		"date_of_birth": subject.DateOfBirth.Format(time.DateOnly),
	})
	if err != nil {
		return domain.BureauReport{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bureau/v1/reports", bytes.NewReader(body))
	if err != nil {
		return domain.BureauReport{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return domain.BureauReport{}, fmt.Errorf("credit bureau request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return domain.BureauReport{}, fmt.Errorf("credit bureau response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return domain.BureauReport{}, fmt.Errorf("credit bureau returned HTTP %d", resp.StatusCode)
	}

	var r response
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&r); err != nil {
		return domain.BureauReport{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	// Every field the policy relies on must be present and plausible. A
	// missing obligations figure must not be read as "no obligations".
	switch {
	case r.ReportRef == "",
		r.ActiveLoans == nil || *r.ActiveLoans < 0,
		r.TotalOutstandingMinor == nil || *r.TotalOutstandingMinor < 0,
		r.MonthlyObligationsMinor == nil || *r.MonthlyObligationsMinor < 0,
		r.WorstDelinquencyDays12m == nil || *r.WorstDelinquencyDays12m < 0,
		r.Score != nil && (*r.Score < 300 || *r.Score > 850):
		return domain.BureauReport{}, fmt.Errorf("%w: missing or out-of-range field", ErrMalformed)
	}

	return domain.BureauReport{
		ID: uuid.New(), Provider: ProviderName, RequestRef: requestRef, ProviderRef: r.ReportRef, FetchedAt: c.now().UTC(),
		Score: r.Score, WorstDelinquencyDays12m: *r.WorstDelinquencyDays12m, MonthlyObligationsMinor: *r.MonthlyObligationsMinor,
		TotalOutstandingMinor: *r.TotalOutstandingMinor, ActiveLoans: *r.ActiveLoans, Raw: raw,
	}, nil
}

var _ app.CreditBureau = (*Client)(nil)
