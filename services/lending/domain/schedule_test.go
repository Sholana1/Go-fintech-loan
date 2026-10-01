package domain

import (
	"errors"
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/platform/money"
)

var start = bizdate.New(2026, 10, 1)

// The worked example from the architecture plan (section 8.7):
// NGN 100,000 over 3 months at 5% per month.
func TestScheduleMatchesThePlansWorkedExample(t *testing.T) {
	s, err := BuildSchedule(10_000_000, 500, 3, start)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ payment, interest, principal int64 }{
		{3_672_086, 500_000, 3_172_086},
		{3_672_086, 341_396, 3_330_690},
		{3_672_085, 174_861, 3_497_224},
	}
	for i, w := range want {
		got := s[i]
		if got.InterestDue != w.interest || got.PrincipalDue != w.principal || got.InterestDue+got.PrincipalDue != w.payment {
			t.Errorf("instalment %d: got interest %d principal %d, want %d %d", i+1, got.InterestDue, got.PrincipalDue, w.interest, w.principal)
		}
	}
	if !s[0].DueDate.Equal(bizdate.New(2026, 11, 1)) || !s[2].DueDate.Equal(bizdate.New(2027, 1, 1)) {
		t.Errorf("due dates wrong: %v %v", s[0].DueDate, s[2].DueDate)
	}
	if !s[1].PeriodStart.Equal(s[0].DueDate) || !s[0].AccrualFrom.Equal(start) {
		t.Error("periods must be contiguous and accrual must start at the period start")
	}
}

func TestInterestRoundsHalfToEven(t *testing.T) {
	// At 5% per month, interest = balance / 20.
	// 1,000,010 kobo -> 50,000.5 -> 50,000 (even); 1,000,030 -> 50,001.5 -> 50,002.
	for _, c := range []struct{ principal, wantInterest int64 }{{1_000_010, 50_000}, {1_000_030, 50_002}, {1_000_020, 50_001}} {
		s, err := BuildSchedule(c.principal, 500, 2, start)
		if err != nil {
			t.Fatal(err)
		}
		if s[0].InterestDue != c.wantInterest {
			t.Errorf("principal %d: first interest %d, want %d", c.principal, s[0].InterestDue, c.wantInterest)
		}
	}
}

func TestZeroRateAndSingleInstalment(t *testing.T) {
	s, err := BuildSchedule(1_000_000, 0, 3, start)
	if err != nil {
		t.Fatal(err)
	}
	if s[0].PrincipalDue != 333_333 || s[1].PrincipalDue != 333_333 || s[2].PrincipalDue != 333_334 || Totals(s).Interest != 0 {
		t.Fatalf("zero-rate schedule wrong: %+v", s)
	}
	one, err := BuildSchedule(1_000_000, 400, 1, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].PrincipalDue != 1_000_000 || one[0].InterestDue != 40_000 {
		t.Fatalf("single instalment wrong: %+v", one)
	}
}

func TestInvalidTermsAreRejected(t *testing.T) {
	for name, f := range map[string]func() error{
		"zero principal": func() error { _, err := BuildSchedule(0, 500, 3, start); return err },
		"zero tenor":     func() error { _, err := BuildSchedule(1_000_000, 500, 0, start); return err },
		"long tenor":     func() error { _, err := BuildSchedule(1_000_000, 500, 61, start); return err },
		"negative rate":  func() error { _, err := BuildSchedule(1_000_000, -1, 3, start); return err },
		"absurd rate":    func() error { _, err := BuildSchedule(1_000_000, 5001, 3, start); return err },
		"fee 100%":       func() error { _, err := OriginationFee(1_000_000, 10_000); return err },
	} {
		if err := f(); !errors.Is(err, ErrInvalidTerms) {
			t.Errorf("%s: want ErrInvalidTerms, got %v", name, err)
		}
	}
}

// Property: for any terms in the supported range the schedule amortises
// exactly and each line follows the stated method.
func TestSchedulePropertiesHoldForRandomTerms(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261001, 7))
	for range 3000 {
		principal := int64(1_000_000 + rng.IntN(49_000_000))
		rateBps := rng.IntN(1500)
		n := 1 + rng.IntN(24)
		s, err := BuildSchedule(principal, rateBps, n, start)
		if err != nil {
			t.Fatalf("P=%d r=%d n=%d: %v", principal, rateBps, n, err)
		}
		level, _ := InstalmentAmount(principal, rateBps, n)
		balance := principal
		for k, inst := range s {
			wantInterest, _ := money.RoundHalfEven(new(big.Rat).Mul(big.NewRat(balance, 1), big.NewRat(int64(rateBps), 10_000)))
			if inst.InterestDue != wantInterest {
				t.Fatalf("P=%d r=%d n=%d k=%d: interest %d, want %d", principal, rateBps, n, k+1, inst.InterestDue, wantInterest)
			}
			if inst.PrincipalDue <= 0 {
				t.Fatalf("P=%d r=%d n=%d k=%d: non-positive principal %d", principal, rateBps, n, k+1, inst.PrincipalDue)
			}
			if k < n-1 && inst.PrincipalDue+inst.InterestDue != level {
				t.Fatalf("P=%d r=%d n=%d k=%d: payment is not level", principal, rateBps, n, k+1)
			}
			if k == n-1 {
				// The final payment absorbs all rounding. Each period rounds
				// by at most one kobo and that error then compounds at the
				// loan rate, so the bound is the future value of one kobo
				// per period: ((1+r)^n - 1) / r, plus n for the level
				// payment's own rounding.
				bound := int64(n)
				if rateBps > 0 {
					r := big.NewRat(int64(rateBps), 10_000)
					fv := new(big.Rat).Quo(new(big.Rat).Sub(ratPow(new(big.Rat).Add(big.NewRat(1, 1), r), n), big.NewRat(1, 1)), r)
					whole, _ := money.RoundHalfEven(fv)
					bound += whole + 1
				}
				if diff := inst.PrincipalDue + inst.InterestDue - level; diff > bound || diff < -bound {
					t.Fatalf("P=%d r=%d n=%d: final payment differs from level by %d kobo (bound %d)", principal, rateBps, n, diff, bound)
				}
			}
			if !inst.DueDate.Equal(bizdate.AddMonths(start, k+1)) {
				t.Fatalf("due date of instalment %d wrong", k+1)
			}
			balance -= inst.PrincipalDue
		}
		if balance != 0 || Totals(s).Principal != principal {
			t.Fatalf("P=%d r=%d n=%d: principal does not amortise to zero (left %d)", principal, rateBps, n, balance)
		}
	}
}

func TestCostOfCredit(t *testing.T) {
	// Without a fee, the cost equals the contractual rate: 5% per month is
	// 60% nominal and (1.05^12 - 1) = 79.59% effective.
	s, _ := BuildSchedule(10_000_000, 500, 3, start)
	payments := []int64{}
	for _, i := range s {
		payments = append(payments, i.PrincipalDue+i.InterestDue)
	}
	c, err := CostOfCredit(10_000_000, payments)
	if err != nil {
		t.Fatal(err)
	}
	if c.NominalBps != 6000 || c.EffectiveBps != 7959 {
		t.Fatalf("no-fee cost %+v, want nominal 6000 effective 7959", c)
	}
	// A 1% fee reduces what the customer receives, so the true cost is higher.
	withFee, err := CostOfCredit(9_900_000, payments)
	if err != nil {
		t.Fatal(err)
	}
	if withFee.EffectiveBps <= c.EffectiveBps {
		t.Fatalf("fee must raise the effective cost: %d vs %d", withFee.EffectiveBps, c.EffectiveBps)
	}
	if _, err := CostOfCredit(10_000_000, []int64{1}); !errors.Is(err, ErrInvalidTerms) {
		t.Fatalf("payments below the amount disbursed must be rejected, got %v", err)
	}
	// Zero-interest loan with no fee costs nothing.
	zero, err := CostOfCredit(900, []int64{300, 300, 300})
	if err != nil || zero.EffectiveBps != 0 {
		t.Fatalf("zero-cost loan: %+v %v", zero, err)
	}
}

func TestPriceOfferAndDisclosureHash(t *testing.T) {
	prod := testProduct()
	terms, err := PriceOffer(prod, 10_000_000, 3, 500, start)
	if err != nil {
		t.Fatal(err)
	}
	if terms.OriginationFeeMinor != 100_000 || terms.NetDisbursementMinor != 9_900_000 ||
		terms.TotalInterestMinor != 1_016_257 || terms.TotalRepayableMinor != 11_016_257 || terms.InstalmentMinor != 3_672_086 {
		t.Fatalf("terms wrong: %+v", terms)
	}
	expires := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	d := BuildDisclosure(prod, terms, expires)
	doc1, h1, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	_, h2, _ := BuildDisclosure(prod, terms, expires).Canonical()
	if h1 != h2 || len(h1) != 64 {
		t.Fatal("the disclosure hash must be deterministic")
	}
	if d.TotalCostOfCreditMinor != 1_116_257 || len(d.Schedule) != 3 || len(doc1) == 0 {
		t.Fatalf("disclosure wrong: %+v", d)
	}
	other, _ := PriceOffer(prod, 10_000_000, 3, 501, start)
	if _, h3, _ := BuildDisclosure(prod, other, expires).Canonical(); h3 == h1 {
		t.Fatal("different terms must hash differently")
	}
}

func testProduct() Product {
	return Product{
		ID: "personal-loan", Version: 1, Currency: money.NGN,
		MinPrincipalMinor: 1_000_000, MaxPrincipalMinor: 50_000_000, PrincipalStepMinor: 100_000,
		TenorsMonths: []int{1, 3, 6}, MinKYCTier: 2, MinAge: 18,
		RateBands:         []RateBand{{"A", 400}, {"B", 600}, {"C", 800}},
		OriginationFeeBps: 100, LateFeeMinor: 50_000, LateFeeGraceDays: 3,
		OfferValidityHours: 24, ApplicationValidityHours: 24,
		ArrearsBuckets:         []ArrearsBucket{{"CURRENT", 0}, {"DPD_1_30", 1}, {"DPD_31_60", 31}, {"DPD_61_90", 61}, {"DPD_OVER_90", 91}},
		WriteOffMinDaysPastDue: 91, RestructureMaxTenorMonths: 12,
		AllowPartialCollection: true, CollectionAttemptsPerDay: 3, CollectionRetryAfterHours: 4,
		DisclosureVersion: "PL-TERMS-1",
	}
}

func testPolicy() Policy {
	return Policy{
		Version: "credit-policy-v1", MinBureauScore: 500,
		ScoreBands:            []ScoreBand{{700, "A"}, {600, "B"}, {500, "C"}},
		MaxDelinquencyDays12m: 60, MaxDebtServiceBps: 4000,
		MaxTotalExposureMinor: 50_000_000, AutoApproveMaxPrincipalMinor: 30_000_000,
		MaxApplications24h: 3,
	}
}
