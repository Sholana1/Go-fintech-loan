package domain

import (
	"math/big"

	"bankplatform.internal/platform/money"
)

// Ineligible returns the reasons, if any, that rule the application out
// without reference to the credit bureau: identity, fraud, and the
// customer's existing relationship. Assessment checks these first so that a
// bureau enquiry (which leaves a footprint on the customer's file and costs
// money) is only made for applications that could be approved.
func Ineligible(prod Product, f Features) []string {
	var reasons []string
	if !f.CustomerActive {
		reasons = append(reasons, ReasonCustomerBlocked)
	}
	if !f.KYCVerified || f.KYCTier < prod.MinKYCTier {
		reasons = append(reasons, ReasonKYCInsufficient)
	}
	if f.AgeYears < prod.MinAge {
		reasons = append(reasons, ReasonUnderage)
	}
	if f.FraudDecision == FraudBlock {
		reasons = append(reasons, ReasonFraudRisk)
	}
	if f.HasActiveLoan {
		reasons = append(reasons, ReasonExistingLoan)
	}
	if f.HasPriorWriteOff {
		reasons = append(reasons, ReasonPriorWriteOff)
	}
	return reasons
}

// Evaluate applies the credit policy. It is a pure function of its inputs.
//
// Order of rules: hard eligibility and fraud first (decline wins), then
// bureau quality, then exposure, then affordability (which may reduce the
// amount), then the auto-approval ceiling (which refers to a human).
func Evaluate(pol Policy, prod Product, f Features) Decision {
	declines := Ineligible(prod, f)
	if f.BureauWorstDelinquencyDays12m > pol.MaxDelinquencyDays12m {
		declines = append(declines, ReasonBureauDelinquency)
	}
	if f.BureauScore != nil && *f.BureauScore < pol.MinBureauScore {
		declines = append(declines, ReasonBureauScoreLow)
	}
	if len(declines) > 0 {
		return Decision{Outcome: OutcomeDecline, ReasonCodes: declines}
	}

	// A thin file cannot be priced automatically. A reviewer decides.
	if f.BureauScore == nil {
		return Decision{Outcome: OutcomeRefer, ReasonCodes: []string{ReasonThinFile}}
	}

	band := ""
	for _, b := range pol.ScoreBands {
		if *f.BureauScore >= b.MinScore {
			band = b.Band
			break
		}
	}
	rateBps, ok := prod.RateFor(band)
	if !ok {
		// Policy.Validate guarantees every band has a rate; refuse to price
		// rather than guess if that guarantee is ever broken.
		return Decision{Outcome: OutcomeRefer, ReasonCodes: []string{ReasonPricingUnavailable}}
	}

	approved, reasons, declined := SizeLoan(pol, prod, f, rateBps)
	if declined != "" {
		return Decision{Outcome: OutcomeDecline, ReasonCodes: []string{declined}, RiskBand: band}
	}

	d := Decision{ApprovedPrincipalMinor: approved, RiskBand: band, MonthlyRateBps: rateBps, ReasonCodes: reasons}
	switch {
	case f.FraudDecision == FraudReview:
		d.Outcome = OutcomeRefer
		d.ReasonCodes = append(d.ReasonCodes, ReasonFraudReview)
	case approved > pol.AutoApproveMaxPrincipalMinor:
		d.Outcome = OutcomeRefer
		d.ReasonCodes = append(d.ReasonCodes, ReasonAboveAutoApprove)
	default:
		d.Outcome = OutcomeApprove
	}
	if d.ReasonCodes == nil {
		d.ReasonCodes = []string{}
	}
	return d
}

// SizeLoan returns the largest principal the policy permits for this
// customer at the given rate: the requested amount, capped by the product
// maximum, the exposure limit and affordability. It returns a decline reason
// when even the product minimum does not fit.
//
// A human reviewer is bound by the same function: manual review can settle
// a referral, but it cannot approve more than SizeLoan allows.
func SizeLoan(pol Policy, prod Product, f Features, rateBps int) (approved int64, reasons []string, declined string) {
	approved = min(f.RequestedPrincipalMinor, prod.MaxPrincipalMinor)

	// Exposure: total principal with us may not exceed the policy cap.
	if room := pol.MaxTotalExposureMinor - f.InternalOutstandingPrincipalMinor; room < approved {
		approved = floorToStep(room, prod.PrincipalStepMinor)
		if approved < prod.MinPrincipalMinor {
			return 0, nil, ReasonExposureLimit
		}
		reasons = append(reasons, ReasonAmountReducedExposure)
	}

	// Affordability: the instalment plus existing obligations must fit
	// within the permitted share of income.
	budget, err := money.MulDivFloor(max(f.StatedMonthlyIncomeMinor, 0), int64(pol.MaxDebtServiceBps), bpsDenominator)
	if err != nil {
		return 0, nil, ReasonAffordability
	}
	affordable := maxAffordablePrincipal(budget-f.BureauMonthlyObligationsMinor, rateBps, f.TenorMonths, prod.PrincipalStepMinor)
	if affordable < approved {
		approved = affordable
		if approved < prod.MinPrincipalMinor {
			return 0, nil, ReasonAffordability
		}
		reasons = append(reasons, ReasonAmountReduced)
	}
	return approved, reasons, ""
}

func floorToStep(amount, step int64) int64 {
	if amount <= 0 {
		return 0
	}
	return amount - amount%step
}

// maxAffordablePrincipal returns the largest principal, in whole steps, whose
// level instalment does not exceed maxInstalment.
//
// It inverts the annuity formula to get a starting point and then steps down
// until the exactly computed instalment fits, so rounding can never approve
// an instalment a kobo above the budget.
func maxAffordablePrincipal(maxInstalment int64, monthlyRateBps, n int, step int64) int64 {
	if maxInstalment <= 0 || n < 1 {
		return 0
	}
	var estimate *big.Rat
	a := new(big.Rat).SetInt64(maxInstalment)
	if monthlyRateBps == 0 {
		estimate = new(big.Rat).Mul(a, new(big.Rat).SetInt64(int64(n)))
	} else {
		r := rate(monthlyRateBps)
		growth := ratPow(new(big.Rat).Add(big.NewRat(1, 1), r), n)
		// P = A * ((1+r)^n - 1) / (r * (1+r)^n)
		estimate = new(big.Rat).Quo(
			new(big.Rat).Mul(a, new(big.Rat).Sub(growth, big.NewRat(1, 1))),
			new(big.Rat).Mul(r, growth))
	}
	whole := new(big.Int).Quo(estimate.Num(), estimate.Denom())
	if !whole.IsInt64() {
		return 0
	}
	p := floorToStep(whole.Int64()+step, step) // start one step high, then walk down
	for p > 0 {
		inst, err := InstalmentAmount(p, monthlyRateBps, n)
		if err == nil && inst <= maxInstalment {
			return p
		}
		p -= step
	}
	return 0
}
