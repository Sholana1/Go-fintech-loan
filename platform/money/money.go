// Package money holds the platform's exact money arithmetic.
//
// Responsibility: represent amounts as integer minor units with a currency,
// and provide the one rounding rule (half-even) every service uses.
// Consistency boundary: pure values; no I/O.
// Failure modes: currency mismatch and int64 overflow are returned as errors,
// never silently wrapped.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// Currency is an ISO 4217 alphabetic code in upper case.
type Currency string

// NGN is the only currency on the customer ledger today (plan assumption A5).
const NGN Currency = "NGN"

var (
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: amount overflows int64")
	ErrInvalidCurrency  = errors.New("money: invalid currency code")
)

// Amount is an exact amount in minor units (kobo for NGN). The zero value is
// not usable because it has no currency; build amounts with New.
type Amount struct {
	minor    int64
	currency Currency
}

// New returns an Amount after validating the currency code shape.
func New(minor int64, c Currency) (Amount, error) {
	if !validCurrency(c) {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, c)
	}
	return Amount{minor: minor, currency: c}, nil
}

// MustNew is for constants and tests; it panics on an invalid currency.
func MustNew(minor int64, c Currency) Amount {
	a, err := New(minor, c)
	if err != nil {
		panic(err)
	}
	return a
}

func validCurrency(c Currency) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func (a Amount) Minor() int64       { return a.minor }
func (a Amount) Currency() Currency { return a.currency }
func (a Amount) IsZero() bool       { return a.minor == 0 }
func (a Amount) IsPositive() bool   { return a.minor > 0 }
func (a Amount) IsNegative() bool   { return a.minor < 0 }

// Add returns a+b, failing on currency mismatch or overflow.
func (a Amount) Add(b Amount) (Amount, error) {
	if a.currency != b.currency {
		return Amount{}, ErrCurrencyMismatch
	}
	sum, ok := AddInt64(a.minor, b.minor)
	if !ok {
		return Amount{}, ErrOverflow
	}
	return Amount{minor: sum, currency: a.currency}, nil
}

// Sub returns a-b, failing on currency mismatch or overflow.
func (a Amount) Sub(b Amount) (Amount, error) {
	if a.currency != b.currency {
		return Amount{}, ErrCurrencyMismatch
	}
	if b.minor == math.MinInt64 {
		return Amount{}, ErrOverflow
	}
	return a.Add(Amount{minor: -b.minor, currency: b.currency})
}

// String renders the amount with two decimal places, e.g. "NGN 1234.50".
// It is for logs and receipts, not for parsing.
func (a Amount) String() string {
	sign := ""
	m := a.minor
	if m < 0 {
		sign = "-"
		m = -m
	}
	return fmt.Sprintf("%s %s%d.%02d", a.currency, sign, m/100, m%100)
}

// AddInt64 adds two int64 values and reports whether the result is exact.
func AddInt64(a, b int64) (int64, bool) {
	c := a + b
	if (c > a) == (b > 0) || b == 0 {
		return c, true
	}
	return 0, false
}

// RoundHalfEven rounds the exact rational r to the nearest integer, with ties
// going to the even neighbour (banker's rounding). This is the platform's
// single rounding rule for converting computed interest and fees to minor
// units; using one rule everywhere is what makes schedules reproducible.
func RoundHalfEven(r *big.Rat) (int64, error) {
	num := new(big.Int).Set(r.Num())
	den := r.Denom() // always > 0

	q, rem := new(big.Int).QuoRem(num, den, new(big.Int)) // truncates toward zero
	twiceRem := new(big.Int).Mul(new(big.Int).Abs(rem), big.NewInt(2))
	switch twiceRem.Cmp(den) {
	case 1: // more than half: round away from zero
		q.Add(q, big.NewInt(int64(num.Sign())))
	case 0: // exactly half: go to the even neighbour
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(int64(num.Sign())))
		}
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}

// MulDivHalfEven computes a*num/den rounded half-even, exactly.
func MulDivHalfEven(a, num, den int64) (int64, error) {
	if den == 0 {
		return 0, errors.New("money: division by zero")
	}
	r := new(big.Rat).SetFrac(
		new(big.Int).Mul(big.NewInt(a), big.NewInt(num)),
		big.NewInt(den),
	)
	return RoundHalfEven(r)
}

// MulDivFloor computes floor(a*num/den) exactly for non-negative inputs.
// Used where a cumulative amount must never exceed its target (accruals).
func MulDivFloor(a, num, den int64) (int64, error) {
	if den <= 0 || a < 0 || num < 0 {
		return 0, errors.New("money: MulDivFloor requires a,num >= 0 and den > 0")
	}
	q := new(big.Int).Quo(
		new(big.Int).Mul(big.NewInt(a), big.NewInt(num)),
		big.NewInt(den),
	)
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}
