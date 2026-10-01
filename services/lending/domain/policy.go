package domain

import (
	"errors"
	"fmt"
	"sort"
)

// Policy is one version of the credit policy. A policy change is a new
// version shipped through review, never an edit in place: each decision
// records the version that produced it and can be replayed against it.
type Policy struct {
	Version string `json:"version"`

	// MinBureauScore: below this the application is declined.
	MinBureauScore int `json:"min_bureau_score"`
	// ScoreBands map a bureau score to a risk band, highest threshold first.
	ScoreBands []ScoreBand `json:"score_bands"`
	// MaxDelinquencyDays12m: worse than this in the last 12 months declines.
	MaxDelinquencyDays12m int `json:"max_delinquency_days_12m"`
	// MaxDebtServiceBps caps (existing monthly obligations + new instalment)
	// as a share of stated monthly income.
	MaxDebtServiceBps int `json:"max_debt_service_bps"`
	// MaxTotalExposureMinor caps the customer's total principal with us.
	MaxTotalExposureMinor int64 `json:"max_total_exposure_minor"`
	// AutoApproveMaxPrincipalMinor: larger approved amounts go to a reviewer.
	AutoApproveMaxPrincipalMinor int64 `json:"auto_approve_max_principal_minor"`
	// MaxApplications24h is the velocity limit used by fraud screening.
	MaxApplications24h int `json:"max_applications_24h"`
}

type ScoreBand struct {
	MinScore int    `json:"min_score"`
	Band     string `json:"band"`
}

var ErrInvalidPolicy = errors.New("invalid credit policy")

func (p Policy) Validate(prod Product) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidPolicy, fmt.Sprintf(format, a...))
	}
	if p.Version == "" {
		return fail("version is required")
	}
	if p.MinBureauScore <= 0 || p.MaxDelinquencyDays12m < 0 {
		return fail("bureau thresholds are required")
	}
	if p.MaxDebtServiceBps <= 0 || p.MaxDebtServiceBps > bpsDenominator {
		return fail("max_debt_service_bps must be between 1 and 10000")
	}
	if p.MaxTotalExposureMinor <= 0 || p.AutoApproveMaxPrincipalMinor <= 0 || p.MaxApplications24h <= 0 {
		return fail("exposure, auto-approve and velocity limits must be positive")
	}
	if len(p.ScoreBands) == 0 {
		return fail("score bands are required")
	}
	if !sort.SliceIsSorted(p.ScoreBands, func(i, j int) bool { return p.ScoreBands[i].MinScore > p.ScoreBands[j].MinScore }) {
		return fail("score bands must be ordered from highest min_score to lowest")
	}
	for _, b := range p.ScoreBands {
		if _, ok := prod.RateFor(b.Band); !ok {
			return fail("score band %q has no rate in product %s v%d", b.Band, prod.ID, prod.Version)
		}
	}
	if p.ScoreBands[len(p.ScoreBands)-1].MinScore > p.MinBureauScore {
		return fail("the lowest score band must cover min_bureau_score")
	}
	return nil
}
