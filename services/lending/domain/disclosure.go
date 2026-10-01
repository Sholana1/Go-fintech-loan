package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Disclosure is the complete statement of terms shown to the customer before
// they accept. Its canonical JSON is stored with the offer and hashed; the
// customer's acceptance must quote the hash, which proves which exact terms
// were accepted.
//
// Field order is fixed by this struct, so the encoding is deterministic.
type Disclosure struct {
	DisclosureVersion string `json:"disclosure_version"`
	ProductID         string `json:"product_id"`
	ProductVersion    int    `json:"product_version"`
	Currency          string `json:"currency"`

	PrincipalMinor       int64 `json:"principal_minor"`
	OriginationFeeMinor  int64 `json:"origination_fee_minor"`
	NetDisbursementMinor int64 `json:"net_disbursement_minor"`
	TenorMonths          int   `json:"tenor_months"`

	MonthlyRateBps         int `json:"monthly_rate_bps"`
	NominalAnnualRateBps   int `json:"nominal_annual_rate_bps"`
	EffectiveAnnualCostBps int `json:"effective_annual_cost_bps"`

	InstalmentMinor     int64 `json:"instalment_minor"`
	TotalInterestMinor  int64 `json:"total_interest_minor"`
	TotalRepayableMinor int64 `json:"total_repayable_minor"`
	// TotalCostOfCreditMinor is interest plus fees: what the loan costs.
	TotalCostOfCreditMinor int64 `json:"total_cost_of_credit_minor"`

	LateFeeMinor     int64 `json:"late_fee_minor"`
	LateFeeGraceDays int   `json:"late_fee_grace_days"`

	Schedule []DisclosedInstalment `json:"schedule"`

	// Method statements, so the customer and a reviewer can reproduce the
	// figures.
	CalculationMethod   string `json:"calculation_method"`
	RoundingMethod      string `json:"rounding_method"`
	EarlyRepaymentTerms string `json:"early_repayment_terms"`
	AllocationOrder     string `json:"allocation_order"`

	OfferExpiresAt time.Time `json:"offer_expires_at"`
}

// DisclosedInstalment is one schedule line as shown in the offer. Due dates
// are fixed at acceptance (instalment k falls due k months after the
// acceptance date), so the offer shows the period number.
type DisclosedInstalment struct {
	Seq            int   `json:"seq"`
	DueAfterMonths int   `json:"due_after_months"`
	PrincipalMinor int64 `json:"principal_minor"`
	InterestMinor  int64 `json:"interest_minor"`
	PaymentMinor   int64 `json:"payment_minor"`
}

// Method statements for product version 1 disclosures.
const (
	MethodCalculation = "Equal monthly instalments. Each period's interest is the opening principal multiplied by the monthly rate; the remainder of the instalment repays principal. The final instalment repays the remaining principal exactly."
	MethodRounding    = "Every amount is rounded to the nearest kobo, with exact halves rounded to the even kobo."
	MethodEarly       = "You may settle the loan in full at any time. You pay outstanding principal, interest earned up to the day before settlement, and any fees. Interest not yet earned is not charged. Partial prepayment of future instalments is not available."
	MethodAllocation  = "Payments are applied to fees, then interest, then principal on amounts already due, oldest first; any remainder is applied to the next instalment."
)

// Canonical returns the deterministic JSON encoding and its SHA-256 hash.
func (d Disclosure) Canonical() (doc []byte, hash string, err error) {
	doc, err = json.Marshal(d)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(doc)
	return doc, hex.EncodeToString(sum[:]), nil
}

// OfferTerms are the priced terms of an approved application.
type OfferTerms struct {
	PrincipalMinor         int64
	TenorMonths            int
	MonthlyRateBps         int
	OriginationFeeMinor    int64
	NetDisbursementMinor   int64
	InstalmentMinor        int64
	TotalInterestMinor     int64
	TotalRepayableMinor    int64
	NominalAnnualRateBps   int
	EffectiveAnnualCostBps int
	Schedule               []Instalment
}

// PriceOffer computes the full terms for an approved principal. The schedule
// it contains is undated in substance (amounts do not depend on the start
// date); asOf only anchors illustrative period boundaries.
func PriceOffer(prod Product, principal int64, tenorMonths, monthlyRateBps int, asOf time.Time) (OfferTerms, error) {
	schedule, err := BuildSchedule(principal, monthlyRateBps, tenorMonths, asOf)
	if err != nil {
		return OfferTerms{}, err
	}
	fee, err := OriginationFee(principal, prod.OriginationFeeBps)
	if err != nil {
		return OfferTerms{}, err
	}
	totals := Totals(schedule)
	payments := make([]int64, len(schedule))
	for i, inst := range schedule {
		payments[i] = inst.PrincipalDue + inst.InterestDue
	}
	cost, err := CostOfCredit(principal-fee, payments)
	if err != nil {
		return OfferTerms{}, err
	}
	return OfferTerms{
		PrincipalMinor: principal, TenorMonths: tenorMonths, MonthlyRateBps: monthlyRateBps,
		OriginationFeeMinor: fee, NetDisbursementMinor: principal - fee,
		InstalmentMinor:    payments[0],
		TotalInterestMinor: totals.Interest, TotalRepayableMinor: totals.Principal + totals.Interest,
		NominalAnnualRateBps: monthlyRateBps * 12, EffectiveAnnualCostBps: cost.EffectiveBps,
		Schedule: schedule,
	}, nil
}

// BuildDisclosure assembles the disclosure for priced terms.
func BuildDisclosure(prod Product, t OfferTerms, expiresAt time.Time) Disclosure {
	d := Disclosure{
		DisclosureVersion: prod.DisclosureVersion, ProductID: prod.ID, ProductVersion: prod.Version, Currency: string(prod.Currency),
		PrincipalMinor: t.PrincipalMinor, OriginationFeeMinor: t.OriginationFeeMinor, NetDisbursementMinor: t.NetDisbursementMinor,
		TenorMonths: t.TenorMonths, MonthlyRateBps: t.MonthlyRateBps,
		NominalAnnualRateBps: t.NominalAnnualRateBps, EffectiveAnnualCostBps: t.EffectiveAnnualCostBps,
		InstalmentMinor: t.InstalmentMinor, TotalInterestMinor: t.TotalInterestMinor, TotalRepayableMinor: t.TotalRepayableMinor,
		TotalCostOfCreditMinor: t.TotalInterestMinor + t.OriginationFeeMinor,
		LateFeeMinor:           prod.LateFeeMinor, LateFeeGraceDays: prod.LateFeeGraceDays,
		CalculationMethod: MethodCalculation, RoundingMethod: MethodRounding,
		EarlyRepaymentTerms: MethodEarly, AllocationOrder: MethodAllocation,
		OfferExpiresAt: expiresAt.UTC().Truncate(time.Second),
	}
	for _, i := range t.Schedule {
		d.Schedule = append(d.Schedule, DisclosedInstalment{
			Seq: i.Seq, DueAfterMonths: i.Seq, PrincipalMinor: i.PrincipalDue, InterestMinor: i.InterestDue,
			PaymentMinor: i.PrincipalDue + i.InterestDue,
		})
	}
	return d
}
