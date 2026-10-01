// Package domain holds the personal-loan product's rules as pure functions:
// how a repayment schedule is computed and rounded, how interest is earned
// day by day, how a repayment is allocated, when a loan is in arrears, and
// what the credit policy decides. Nothing here performs I/O or reads a clock;
// every function takes the business date it needs.
//
// All amounts are int64 minor units (kobo). Intermediate arithmetic uses
// exact rationals and the platform's single rounding rule (half-even), so a
// schedule can be recomputed years later and match to the kobo.
package domain

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/platform/money"
)

// Limits that keep inputs within a range the arithmetic is tested for.
const (
	MaxTenorMonths   = 60
	MaxMonthlyRateBp = 5000 // 50% per month: far above any product; a guard against bad configuration
	bpsDenominator   = 10_000
)

var ErrInvalidTerms = errors.New("invalid loan terms")

// Instalment is one row of a loan's repayment schedule together with what
// has been paid and earned against it.
//
// Interest for a period is fixed when the schedule is built: it is the
// period's opening principal times the monthly rate. It is earned evenly
// over the days of the period (see EntitledInterest).
type Instalment struct {
	Seq         int
	PeriodStart time.Time // business date the period opens
	DueDate     time.Time // business date the instalment falls due

	PrincipalDue int64
	InterestDue  int64
	FeesDue      int64 // late fees assessed against this instalment

	PrincipalPaid int64
	InterestPaid  int64
	FeesPaid      int64

	// InterestAccrued is the interest for this instalment already recognised
	// as income in the ledger by the daily accrual job.
	InterestAccrued int64
	// AccrualFrom and AccrualBase describe how interest is earned: AccrualBase
	// is already earned on AccrualFrom, and the rest (InterestDue-AccrualBase)
	// is earned evenly from AccrualFrom to DueDate. For an ordinary instalment
	// AccrualFrom is PeriodStart and AccrualBase is zero; a restructured
	// schedule carries previously earned interest in AccrualBase.
	AccrualFrom time.Time
	AccrualBase int64
}

// Outstanding amounts of one instalment.
func (i Instalment) PrincipalOutstanding() int64 { return i.PrincipalDue - i.PrincipalPaid }

func (i Instalment) FeesOutstanding() int64 { return i.FeesDue - i.FeesPaid }

// FullyPaid reports whether nothing scheduled on this instalment remains.
func (i Instalment) FullyPaid() bool {
	return i.PrincipalPaid >= i.PrincipalDue && i.InterestPaid >= i.InterestDue && i.FeesPaid >= i.FeesDue
}

func rate(monthlyRateBps int) *big.Rat { return big.NewRat(int64(monthlyRateBps), bpsDenominator) }

// InstalmentAmount returns the level payment that amortises principal over n
// monthly periods at the given monthly rate:
//
//	A = P * r / (1 - (1+r)^-n)
//
// computed exactly and rounded half-even to the kobo. With a zero rate it is
// P/n.
func InstalmentAmount(principal int64, monthlyRateBps, n int) (int64, error) {
	if err := validateTerms(principal, monthlyRateBps, n); err != nil {
		return 0, err
	}
	p := new(big.Rat).SetInt64(principal)
	if monthlyRateBps == 0 {
		return money.RoundHalfEven(new(big.Rat).Quo(p, new(big.Rat).SetInt64(int64(n))))
	}
	r := rate(monthlyRateBps)
	growth := ratPow(new(big.Rat).Add(big.NewRat(1, 1), r), n) // (1+r)^n
	num := new(big.Rat).Mul(new(big.Rat).Mul(p, r), growth)
	den := new(big.Rat).Sub(growth, big.NewRat(1, 1))
	return money.RoundHalfEven(new(big.Rat).Quo(num, den))
}

func ratPow(base *big.Rat, n int) *big.Rat {
	out := big.NewRat(1, 1)
	for range n {
		out.Mul(out, base)
	}
	return out
}

func validateTerms(principal int64, monthlyRateBps, n int) error {
	switch {
	case principal <= 0:
		return fmt.Errorf("%w: principal must be positive", ErrInvalidTerms)
	case n < 1 || n > MaxTenorMonths:
		return fmt.Errorf("%w: tenor must be between 1 and %d months", ErrInvalidTerms, MaxTenorMonths)
	case monthlyRateBps < 0 || monthlyRateBps > MaxMonthlyRateBp:
		return fmt.Errorf("%w: monthly rate out of range", ErrInvalidTerms)
	}
	return nil
}

// BuildSchedule returns the repayment schedule for a loan accepted on start.
//
// Method (disclosed to the customer in the offer):
//   - Equal monthly instalments (see InstalmentAmount).
//   - Each period's interest = opening principal x monthly rate, rounded
//     half-even to the kobo.
//   - Each period's principal = instalment - interest.
//   - The final instalment repays whatever principal remains, so rounding
//     never leaves a residual: the principal column sums to the loan exactly.
//   - Instalment k falls due k months after start (day-of-month preserved,
//     clamped to month end).
func BuildSchedule(principal int64, monthlyRateBps, n int, start time.Time) ([]Instalment, error) {
	level, err := InstalmentAmount(principal, monthlyRateBps, n)
	if err != nil {
		return nil, err
	}
	r := rate(monthlyRateBps)
	balance := principal
	periodStart := start
	out := make([]Instalment, 0, n)
	for k := 1; k <= n; k++ {
		interest, err := money.RoundHalfEven(new(big.Rat).Mul(new(big.Rat).SetInt64(balance), r))
		if err != nil {
			return nil, err
		}
		princ := level - interest
		if k == n {
			princ = balance
		}
		if princ <= 0 || princ > balance {
			// Cannot happen for terms within the validated range; guards
			// against a schedule that would never amortise.
			return nil, fmt.Errorf("%w: schedule does not amortise at instalment %d", ErrInvalidTerms, k)
		}
		due := bizdate.AddMonths(start, k)
		out = append(out, Instalment{
			Seq: k, PeriodStart: periodStart, DueDate: due,
			PrincipalDue: princ, InterestDue: interest,
			AccrualFrom: periodStart,
		})
		balance -= princ
		periodStart = due
	}
	return out, nil
}

// ScheduleTotals summarises a schedule.
type ScheduleTotals struct {
	Principal int64
	Interest  int64
	Fees      int64
}

func Totals(insts []Instalment) ScheduleTotals {
	var t ScheduleTotals
	for _, i := range insts {
		t.Principal += i.PrincipalDue
		t.Interest += i.InterestDue
		t.Fees += i.FeesDue
	}
	return t
}
