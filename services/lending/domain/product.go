package domain

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"bankplatform.internal/platform/money"
)

// Product is one version of the personal-loan product. Every number that
// affects what a customer pays lives here, in versioned configuration, never
// in code. A loan records the product version it was written under and is
// serviced under that version for life.
type Product struct {
	ID       string         `json:"id"`
	Version  int            `json:"version"`
	Currency money.Currency `json:"currency"`

	MinPrincipalMinor int64 `json:"min_principal_minor"`
	MaxPrincipalMinor int64 `json:"max_principal_minor"`
	// PrincipalStepMinor is the granularity of approved amounts when the
	// policy reduces a request for affordability.
	PrincipalStepMinor int64 `json:"principal_step_minor"`
	TenorsMonths       []int `json:"tenors_months"`

	MinKYCTier int `json:"min_kyc_tier"`
	MinAge     int `json:"min_age"`

	// RateBands maps a risk band from the credit policy to a monthly rate.
	RateBands []RateBand `json:"rate_bands"`
	// OriginationFeeBps is charged once on the principal and deducted from
	// the disbursement.
	OriginationFeeBps int `json:"origination_fee_bps"`
	// LateFeeMinor is charged once per instalment that remains unpaid for
	// more than LateFeeGraceDays. Zero disables late fees.
	LateFeeMinor     int64 `json:"late_fee_minor"`
	LateFeeGraceDays int   `json:"late_fee_grace_days"`

	OfferValidityHours int `json:"offer_validity_hours"`
	// ApplicationValidityHours bounds how long an application may wait on an
	// unavailable dependency before it expires.
	ApplicationValidityHours int `json:"application_validity_hours"`

	// ArrearsBuckets classify a loan by days past due, for operational
	// reporting. They are internal labels; prudential classification and
	// provisioning percentages are owned by finance and are not encoded here.
	ArrearsBuckets []ArrearsBucket `json:"arrears_buckets"`
	// WriteOffMinDaysPastDue is the earliest a write-off may be proposed.
	WriteOffMinDaysPastDue int `json:"write_off_min_days_past_due"`
	// RestructureMaxTenorMonths bounds the tenor of a restructured schedule.
	RestructureMaxTenorMonths int `json:"restructure_max_tenor_months"`

	// AllowPartialCollection lets the scheduled debit take what is available
	// when the full due amount is not.
	AllowPartialCollection    bool `json:"allow_partial_collection"`
	CollectionAttemptsPerDay  int  `json:"collection_attempts_per_day"`
	CollectionRetryAfterHours int  `json:"collection_retry_after_hours"`

	// DisclosureVersion identifies the wording of the terms shown to the
	// customer; it is part of the hashed acceptance evidence.
	DisclosureVersion string `json:"disclosure_version"`
}

type RateBand struct {
	Band           string `json:"band"`
	MonthlyRateBps int    `json:"monthly_rate_bps"`
}

type ArrearsBucket struct {
	Name           string `json:"name"`
	MinDaysPastDue int    `json:"min_days_past_due"`
}

var ErrInvalidProduct = errors.New("invalid product configuration")

// Validate checks the configuration at startup so a bad value stops the
// service instead of mispricing a loan.
func (p Product) Validate() error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidProduct, fmt.Sprintf(format, a...))
	}
	if p.ID == "" || p.Version <= 0 || p.DisclosureVersion == "" {
		return fail("id, version and disclosure_version are required")
	}
	if _, err := money.New(0, p.Currency); err != nil {
		return fail("currency: %v", err)
	}
	if p.MinPrincipalMinor <= 0 || p.MaxPrincipalMinor < p.MinPrincipalMinor || p.PrincipalStepMinor <= 0 {
		return fail("principal limits and step must be positive and ordered")
	}
	if len(p.TenorsMonths) == 0 {
		return fail("at least one tenor is required")
	}
	for _, t := range p.TenorsMonths {
		if t < 1 || t > MaxTenorMonths {
			return fail("tenor %d is out of range", t)
		}
	}
	if p.MinKYCTier < 1 || p.MinKYCTier > 3 || p.MinAge < 18 {
		return fail("min_kyc_tier must be 1-3 and min_age at least 18")
	}
	if len(p.RateBands) == 0 {
		return fail("at least one rate band is required")
	}
	for _, b := range p.RateBands {
		if b.Band == "" || b.MonthlyRateBps < 0 || b.MonthlyRateBps > MaxMonthlyRateBp {
			return fail("rate band %q is invalid", b.Band)
		}
	}
	if p.OriginationFeeBps < 0 || p.OriginationFeeBps >= bpsDenominator || p.LateFeeMinor < 0 || p.LateFeeGraceDays < 0 {
		return fail("fees must be non-negative and origination fee below 100%%")
	}
	if p.OfferValidityHours <= 0 || p.ApplicationValidityHours <= 0 {
		return fail("validity periods must be positive")
	}
	if len(p.ArrearsBuckets) == 0 || p.ArrearsBuckets[0].MinDaysPastDue != 0 {
		return fail("arrears buckets must start at 0 days past due")
	}
	for i := 1; i < len(p.ArrearsBuckets); i++ {
		if p.ArrearsBuckets[i].MinDaysPastDue <= p.ArrearsBuckets[i-1].MinDaysPastDue {
			return fail("arrears buckets must be strictly increasing")
		}
	}
	if p.WriteOffMinDaysPastDue <= 0 || p.RestructureMaxTenorMonths < 1 || p.RestructureMaxTenorMonths > MaxTenorMonths {
		return fail("write-off and restructure limits are required")
	}
	if p.CollectionAttemptsPerDay < 1 || p.CollectionRetryAfterHours < 1 {
		return fail("collection limits must be positive")
	}
	return nil
}

// RateFor returns the monthly rate for a risk band.
func (p Product) RateFor(band string) (int, bool) {
	for _, b := range p.RateBands {
		if b.Band == band {
			return b.MonthlyRateBps, true
		}
	}
	return 0, false
}

func (p Product) AllowsTenor(months int) bool { return slices.Contains(p.TenorsMonths, months) }

func (p Product) OfferValidity() time.Duration {
	return time.Duration(p.OfferValidityHours) * time.Hour
}

func (p Product) ApplicationValidity() time.Duration {
	return time.Duration(p.ApplicationValidityHours) * time.Hour
}

// Bucket returns the arrears bucket for a number of days past due.
func (p Product) Bucket(daysPastDue int) string {
	name := p.ArrearsBuckets[0].Name
	for _, b := range p.ArrearsBuckets {
		if daysPastDue >= b.MinDaysPastDue {
			name = b.Name
		}
	}
	return name
}
