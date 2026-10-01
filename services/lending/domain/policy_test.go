package domain

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func intp(v int) *int { return &v }

func goodFeatures() Features {
	return Features{
		KYCVerified: true, KYCTier: 2, CustomerActive: true, AgeYears: 30,
		FraudDecision: FraudClear, BureauScore: intp(720),
		StatedMonthlyIncomeMinor: 30_000_000, // NGN 300,000
		RequestedPrincipalMinor:  10_000_000, TenorMonths: 3,
	}
}

func TestPolicyApprovesACleanApplicationAtTheBandRate(t *testing.T) {
	d := Evaluate(testPolicy(), testProduct(), goodFeatures())
	if d.Outcome != OutcomeApprove || d.ApprovedPrincipalMinor != 10_000_000 || d.RiskBand != "A" || d.MonthlyRateBps != 400 || len(d.ReasonCodes) != 0 {
		t.Fatalf("unexpected decision %+v", d)
	}
}

func TestPolicyRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Features)
		outcome Outcome
		reason  string
	}{
		{"kyc unverified", func(f *Features) { f.KYCVerified = false }, OutcomeDecline, ReasonKYCInsufficient},
		{"kyc tier too low", func(f *Features) { f.KYCTier = 1 }, OutcomeDecline, ReasonKYCInsufficient},
		{"blocked customer", func(f *Features) { f.CustomerActive = false }, OutcomeDecline, ReasonCustomerBlocked},
		{"fraud block", func(f *Features) { f.FraudDecision = FraudBlock }, OutcomeDecline, ReasonFraudRisk},
		{"fraud review", func(f *Features) { f.FraudDecision = FraudReview }, OutcomeRefer, ReasonFraudReview},
		{"existing loan", func(f *Features) { f.HasActiveLoan = true }, OutcomeDecline, ReasonExistingLoan},
		{"prior write-off", func(f *Features) { f.HasPriorWriteOff = true }, OutcomeDecline, ReasonPriorWriteOff},
		{"score below minimum", func(f *Features) { f.BureauScore = intp(499) }, OutcomeDecline, ReasonBureauScoreLow},
		{"delinquency", func(f *Features) { f.BureauWorstDelinquencyDays12m = 61 }, OutcomeDecline, ReasonBureauDelinquency},
		{"thin file", func(f *Features) { f.BureauScore = nil }, OutcomeRefer, ReasonThinFile},
		{"no income", func(f *Features) { f.StatedMonthlyIncomeMinor = 0 }, OutcomeDecline, ReasonAffordability},
		{"above auto-approval", func(f *Features) {
			f.RequestedPrincipalMinor = 40_000_000
			f.StatedMonthlyIncomeMinor = 500_000_000
		}, OutcomeRefer, ReasonAboveAutoApprove},
	}
	for _, c := range cases {
		f := goodFeatures()
		c.mutate(&f)
		d := Evaluate(testPolicy(), testProduct(), f)
		if d.Outcome != c.outcome || !slices.Contains(d.ReasonCodes, c.reason) {
			t.Errorf("%s: got %s %v, want %s with %s", c.name, d.Outcome, d.ReasonCodes, c.outcome, c.reason)
		}
		if d.Outcome == OutcomeDecline && d.ApprovedPrincipalMinor != 0 {
			t.Errorf("%s: a decline must not carry an approved amount", c.name)
		}
	}
}

func TestPolicyBoundariesAreInclusiveAsDocumented(t *testing.T) {
	pol, prod := testPolicy(), testProduct()
	f := goodFeatures()
	f.BureauScore = intp(500) // exactly the minimum: accepted, lowest band
	if d := Evaluate(pol, prod, f); d.Outcome != OutcomeApprove || d.RiskBand != "C" || d.MonthlyRateBps != 800 {
		t.Fatalf("score 500: %+v", d)
	}
	f.BureauScore = intp(699)
	if d := Evaluate(pol, prod, f); d.RiskBand != "B" {
		t.Fatalf("score 699 should be band B: %+v", d)
	}
	f.BureauScore = intp(700)
	if d := Evaluate(pol, prod, f); d.RiskBand != "A" {
		t.Fatalf("score 700 should be band A: %+v", d)
	}
	f.BureauWorstDelinquencyDays12m = 60 // exactly the maximum: accepted
	if d := Evaluate(pol, prod, f); d.Outcome != OutcomeApprove {
		t.Fatalf("delinquency of exactly 60 days: %+v", d)
	}
}

// The approved amount is the largest whole step whose instalment fits the
// affordability budget; one more step would not fit.
func TestAffordabilityReducesToTheLargestAmountThatFits(t *testing.T) {
	pol, prod := testPolicy(), testProduct()
	f := goodFeatures()
	f.RequestedPrincipalMinor = 30_000_000
	f.StatedMonthlyIncomeMinor = 10_000_000     // budget 40% = 4,000,000
	f.BureauMonthlyObligationsMinor = 1_000_000 // room 3,000,000 per month

	d := Evaluate(pol, prod, f)
	if d.Outcome != OutcomeApprove || !slices.Contains(d.ReasonCodes, ReasonAmountReduced) {
		t.Fatalf("expected a reduced approval, got %+v", d)
	}
	if d.ApprovedPrincipalMinor%prod.PrincipalStepMinor != 0 || d.ApprovedPrincipalMinor >= f.RequestedPrincipalMinor {
		t.Fatalf("approved %d is not a reduced whole step", d.ApprovedPrincipalMinor)
	}
	fits, _ := InstalmentAmount(d.ApprovedPrincipalMinor, d.MonthlyRateBps, f.TenorMonths)
	tooMuch, _ := InstalmentAmount(d.ApprovedPrincipalMinor+prod.PrincipalStepMinor, d.MonthlyRateBps, f.TenorMonths)
	if fits > 3_000_000 || tooMuch <= 3_000_000 {
		t.Fatalf("approved %d: instalment %d must fit 3,000,000 and the next step (%d) must not", d.ApprovedPrincipalMinor, fits, tooMuch)
	}

	// If even the product minimum does not fit, decline.
	f.BureauMonthlyObligationsMinor = 3_900_000
	if d := Evaluate(pol, prod, f); d.Outcome != OutcomeDecline || !slices.Contains(d.ReasonCodes, ReasonAffordability) {
		t.Fatalf("expected an affordability decline, got %+v", d)
	}
}

func TestExposureLimitReducesOrDeclines(t *testing.T) {
	pol, prod := testPolicy(), testProduct()
	f := goodFeatures()
	f.StatedMonthlyIncomeMinor = 500_000_000
	f.RequestedPrincipalMinor = 20_000_000
	f.InternalOutstandingPrincipalMinor = 35_000_000 // room 15,000,000 under the 50,000,000 cap
	d := Evaluate(pol, prod, f)
	if d.Outcome != OutcomeApprove || d.ApprovedPrincipalMinor != 15_000_000 || !slices.Contains(d.ReasonCodes, ReasonAmountReducedExposure) {
		t.Fatalf("unexpected %+v", d)
	}
	f.InternalOutstandingPrincipalMinor = 49_500_000 // room below the product minimum
	if d := Evaluate(pol, prod, f); d.Outcome != OutcomeDecline || !slices.Contains(d.ReasonCodes, ReasonExposureLimit) {
		t.Fatalf("unexpected %+v", d)
	}
}

// A stored decision snapshot can be replayed and gives the same decision.
func TestDecisionIsReproducibleFromItsStoredFeatures(t *testing.T) {
	pol, prod := testPolicy(), testProduct()
	variants := []func(*Features){
		func(*Features) {},
		func(f *Features) { f.BureauScore = nil },
		func(f *Features) { f.BureauScore = intp(610); f.BureauMonthlyObligationsMinor = 11_000_000 },
		func(f *Features) { f.FraudDecision = FraudReview; f.FraudSignals = []string{"VELOCITY"} },
		func(f *Features) { f.HasActiveLoan = true; f.KYCTier = 1 },
	}
	for i, mutate := range variants {
		f := goodFeatures()
		mutate(&f)
		original := Evaluate(pol, prod, f)

		stored, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		var replayed Features
		if err := json.Unmarshal(stored, &replayed); err != nil {
			t.Fatal(err)
		}
		if again := Evaluate(pol, prod, replayed); !reflect.DeepEqual(original, again) {
			t.Errorf("variant %d: replay gave %+v, original %+v", i, again, original)
		}
	}
}

func TestStateMachines(t *testing.T) {
	if !AppOffered.CanTransition(AppAccepted) || AppDeclined.CanTransition(AppOffered) || AppDisbursed.CanTransition(AppAccepted) ||
		AppExpired.CanTransition(AppAccepted) || AppAccepted.CanTransition(AppCancelled) {
		t.Fatal("application transitions are wrong")
	}
	if !LoanInArrears.CanTransition(LoanWrittenOff) || LoanActive.CanTransition(LoanWrittenOff) || LoanClosed.CanTransition(LoanActive) {
		t.Fatal("loan transitions are wrong")
	}
	// The payout machine must never allow a silent refund or resend.
	if PayoutUnknown.CanTransition(PayoutReady) || PayoutUnknown.CanTransition(PayoutSending) || PayoutSending.CanTransition(PayoutHeld) ||
		PayoutSucceeded.CanTransition(PayoutFailed) || PayoutSettled.CanTransition(PayoutFailed) || PayoutFailed.CanTransition(PayoutReady) {
		t.Fatal("payout transitions allow an unsafe move")
	}
	if !PayoutSending.CanTransition(PayoutUnknown) || !PayoutUnknown.CanTransition(PayoutSucceeded) || !PayoutUnknown.CanTransition(PayoutFailed) {
		t.Fatal("payout transitions are missing a required move")
	}
	if err := ValidateTransition(PayoutUnknown, PayoutSending); err == nil {
		t.Fatal("ValidateTransition must reject an unknown -> sending move")
	}
}

func TestProductAndPolicyValidation(t *testing.T) {
	prod, pol := testProduct(), testPolicy()
	if err := prod.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := pol.Validate(prod); err != nil {
		t.Fatal(err)
	}
	bad := prod
	bad.RateBands = []RateBand{{"A", 400}}
	if err := pol.Validate(bad); err == nil {
		t.Fatal("a policy band without a product rate must be rejected")
	}
	bad = prod
	bad.ArrearsBuckets = []ArrearsBucket{{"X", 5}}
	if err := bad.Validate(); err == nil {
		t.Fatal("buckets must start at zero")
	}
	badPol := pol
	badPol.ScoreBands = []ScoreBand{{500, "C"}, {700, "A"}}
	if err := badPol.Validate(prod); err == nil {
		t.Fatal("score bands must be ordered")
	}
	badPol = pol
	badPol.MinBureauScore = 400 // lower than the lowest band: scores 400-499 would have no price
	if err := badPol.Validate(prod); err != nil {
		// lowest band 500 > min 400 means some passing scores have no band
		return
	}
	t.Fatal("min score below the lowest band must be rejected")
}
