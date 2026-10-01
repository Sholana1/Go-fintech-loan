package money

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

func TestRoundHalfEven(t *testing.T) {
	cases := []struct {
		num, den int64
		want     int64
	}{
		{5, 2, 2},           // 2.5 -> 2 (even)
		{7, 2, 4},           // 3.5 -> 4 (even)
		{-5, 2, -2},         // -2.5 -> -2
		{-7, 2, -4},         // -3.5 -> -4
		{246575, 1000, 247}, // 246.575 -> 247 (not a tie)
		{2465, 10, 246},     // 246.5 -> 246 (tie, even)
		{2475, 10, 248},     // 247.5 -> 248 (tie, even)
		{1, 3, 0},
		{2, 3, 1},
		{-2, 3, -1},
		{0, 7, 0},
		{10, 5, 2},
	}
	for _, c := range cases {
		got, err := RoundHalfEven(big.NewRat(c.num, c.den))
		if err != nil {
			t.Fatalf("%d/%d: %v", c.num, c.den, err)
		}
		if got != c.want {
			t.Errorf("RoundHalfEven(%d/%d) = %d, want %d", c.num, c.den, got, c.want)
		}
	}
}

func TestAddSubDetectOverflowAndMismatch(t *testing.T) {
	a := MustNew(math.MaxInt64, NGN)
	if _, err := a.Add(MustNew(1, NGN)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow, got %v", err)
	}
	if _, err := a.Add(MustNew(1, "USD")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	b := MustNew(math.MinInt64, NGN)
	if _, err := MustNew(0, NGN).Sub(b); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want overflow on negating MinInt64, got %v", err)
	}
	got, err := MustNew(150, NGN).Sub(MustNew(50, NGN))
	if err != nil || got.Minor() != 100 {
		t.Fatalf("150-50 = %v, %v", got, err)
	}
}

func TestNewRejectsBadCurrency(t *testing.T) {
	for _, c := range []Currency{"", "ngn", "NG", "NAIRA", "N1N"} {
		if _, err := New(1, c); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("currency %q: want ErrInvalidCurrency, got %v", c, err)
		}
	}
}

func TestMulDiv(t *testing.T) {
	// 200,000.00 NGN at 15% for 1/365 of a year = 82.19178... -> 8219 kobo
	got, err := MulDivHalfEven(20_000_000, 15, 100*365)
	if err != nil || got != 8219 {
		t.Fatalf("got %d, %v", got, err)
	}
	fl, err := MulDivFloor(500_000, 7, 30) // 116666.66 -> 116666
	if err != nil || fl != 116666 {
		t.Fatalf("got %d, %v", fl, err)
	}
	if _, err := MulDivFloor(-1, 1, 1); err == nil {
		t.Fatal("negative input must be rejected")
	}
}

func TestString(t *testing.T) {
	if s := MustNew(-123450, NGN).String(); s != "NGN -1234.50" {
		t.Fatalf("got %q", s)
	}
	if s := MustNew(5, NGN).String(); s != "NGN 0.05" {
		t.Fatalf("got %q", s)
	}
}
