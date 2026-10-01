package domain

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"bankplatform.internal/platform/money"
)

func ngn(minor int64) money.Amount { return money.MustNew(minor, money.NGN) }

func TestValidateLines(t *testing.T) {
	ok := []Line{
		{"CUST:a:MAIN", Debit, ngn(1000)},
		{"CUST:b:MAIN", Credit, ngn(900)},
		{"SYS:FEE_INCOME", Credit, ngn(100)},
	}
	if err := ValidateLines(ok); err != nil {
		t.Fatalf("balanced journal rejected: %v", err)
	}

	cases := map[string]struct {
		lines []Line
		want  error
	}{
		"one line":     {[]Line{{"CUST:a:MAIN", Debit, ngn(1)}}, ErrInvalid},
		"unbalanced":   {[]Line{{"CUST:a:MAIN", Debit, ngn(100)}, {"CUST:b:MAIN", Credit, ngn(99)}}, ErrUnbalanced},
		"zero amount":  {[]Line{{"CUST:a:MAIN", Debit, ngn(0)}, {"CUST:b:MAIN", Credit, ngn(0)}}, ErrInvalid},
		"negative":     {[]Line{{"CUST:a:MAIN", Debit, ngn(-5)}, {"CUST:b:MAIN", Credit, ngn(-5)}}, ErrInvalid},
		"bad code":     {[]Line{{"a", Debit, ngn(5)}, {"CUST:b:MAIN", Credit, ngn(5)}}, ErrInvalid},
		"no direction": {[]Line{{"CUST:a:MAIN", "", ngn(5)}, {"CUST:b:MAIN", Credit, ngn(5)}}, ErrInvalid},
		"per-currency": {[]Line{
			{"CUST:a:MAIN", Debit, ngn(100)},
			{"CUST:b:MAIN", Credit, money.MustNew(100, "USD")},
		}, ErrUnbalanced},
	}
	for name, c := range cases {
		if err := ValidateLines(c.lines); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", name, c.want, err)
		}
	}
}

func TestFingerprintIgnoresOrderButNotContent(t *testing.T) {
	a := []Line{{"CUST:a:MAIN", Debit, ngn(1000)}, {"CUST:b:MAIN", Credit, ngn(1000)}}
	b := []Line{a[1], a[0]}
	if !bytes.Equal(Fingerprint("X_TYPE", a), Fingerprint("X_TYPE", b)) {
		t.Fatal("line order must not change the fingerprint")
	}
	c := []Line{{"CUST:a:MAIN", Debit, ngn(1001)}, {"CUST:b:MAIN", Credit, ngn(1001)}}
	if bytes.Equal(Fingerprint("X_TYPE", a), Fingerprint("X_TYPE", c)) {
		t.Fatal("amount must change the fingerprint")
	}
	if bytes.Equal(Fingerprint("X_TYPE", a), Fingerprint("Y_TYPE", a)) {
		t.Fatal("journal type must change the fingerprint")
	}
}

func TestBusinessDateIsLagos(t *testing.T) {
	// 23:30 UTC on 30 Sep is 00:30 on 1 Oct in Lagos.
	got := BusinessDate(time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC))
	want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
