package domain

import (
	"fmt"
	"math/big"

	"bankplatform.internal/platform/money"
)

// OriginationFee returns the up-front fee: principal x feeBps, half-even.
func OriginationFee(principal int64, feeBps int) (int64, error) {
	if feeBps < 0 || feeBps >= bpsDenominator {
		return 0, fmt.Errorf("%w: origination fee out of range", ErrInvalidTerms)
	}
	return money.MulDivHalfEven(principal, int64(feeBps), bpsDenominator)
}

// AnnualisedCost expresses the total cost of credit as yearly rates so that
// offers with different fees and tenors can be compared.
type AnnualisedCost struct {
	// NominalBps is the monthly internal rate of return x 12.
	NominalBps int
	// EffectiveBps is (1 + monthly IRR)^12 - 1: the compounded yearly cost.
	EffectiveBps int
}

// CostOfCredit solves for the monthly rate i at which the present value of
// the payments equals the cash the customer actually receives:
//
//	net = sum_k payment_k / (1+i)^k
//
// It therefore includes the origination fee, which the nominal rate alone
// does not. The solution is found by bisection on exact rationals to a
// resolution of 1e-8 per month; no floating point is involved.
func CostOfCredit(netDisbursed int64, payments []int64) (AnnualisedCost, error) {
	if netDisbursed <= 0 || len(payments) == 0 {
		return AnnualisedCost{}, fmt.Errorf("%w: cost of credit needs a positive net amount and payments", ErrInvalidTerms)
	}
	var total int64
	for _, p := range payments {
		total += p
	}
	if total < netDisbursed {
		return AnnualisedCost{}, fmt.Errorf("%w: payments are less than the amount disbursed", ErrInvalidTerms)
	}

	const scale = 100_000_000 // i is searched in units of 1e-8
	net := new(big.Rat).SetInt64(netDisbursed)
	pv := func(units int64) *big.Rat {
		growth := new(big.Rat).Add(big.NewRat(1, 1), big.NewRat(units, scale))
		discount := big.NewRat(1, 1)
		sum := new(big.Rat)
		for _, p := range payments {
			discount.Quo(discount, growth)
			sum.Add(sum, new(big.Rat).Mul(new(big.Rat).SetInt64(p), discount))
		}
		return sum
	}

	lo, hi := int64(0), int64(10*scale) // 0% to 1000% per month
	if pv(hi).Cmp(net) > 0 {
		return AnnualisedCost{}, fmt.Errorf("%w: cost of credit exceeds the supported range", ErrInvalidTerms)
	}
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		if pv(mid).Cmp(net) > 0 {
			lo = mid // discounting too little: the rate is higher
		} else {
			hi = mid
		}
	}
	monthly := big.NewRat(hi, scale)

	nominal, err := money.RoundHalfEven(new(big.Rat).Mul(monthly, big.NewRat(12*bpsDenominator, 1)))
	if err != nil {
		return AnnualisedCost{}, err
	}
	compounded := new(big.Rat).Sub(ratPow(new(big.Rat).Add(big.NewRat(1, 1), monthly), 12), big.NewRat(1, 1))
	effective, err := money.RoundHalfEven(new(big.Rat).Mul(compounded, big.NewRat(bpsDenominator, 1)))
	if err != nil {
		return AnnualisedCost{}, err
	}
	return AnnualisedCost{NominalBps: int(nominal), EffectiveBps: int(effective)}, nil
}
