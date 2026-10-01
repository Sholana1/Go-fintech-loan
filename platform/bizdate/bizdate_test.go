package bizdate

import (
	"testing"
	"time"
)

func TestOfUsesLagosMidnight(t *testing.T) {
	// 23:30 UTC on 30 September is 00:30 on 1 October in Lagos.
	if got := Of(time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC)); !got.Equal(New(2026, 10, 1)) {
		t.Fatalf("got %v", got)
	}
	// 22:59 UTC is still 30 September in Lagos.
	if got := Of(time.Date(2026, 9, 30, 22, 59, 0, 0, time.UTC)); !got.Equal(New(2026, 9, 30)) {
		t.Fatalf("got %v", got)
	}
	if !Of(StartOf(New(2026, 10, 1))).Equal(New(2026, 10, 1)) {
		t.Fatal("StartOf must fall on its own business date")
	}
	if !Of(StartOf(New(2026, 10, 1)).Add(-time.Nanosecond)).Equal(New(2026, 9, 30)) {
		t.Fatal("the instant before StartOf belongs to the previous date")
	}
}

func TestAddMonths(t *testing.T) {
	cases := []struct {
		from time.Time
		n    int
		want time.Time
	}{
		{New(2026, 1, 31), 1, New(2026, 2, 28)},
		{New(2026, 1, 31), 2, New(2026, 3, 31)}, // not 28: anchored on the original day
		{New(2028, 1, 31), 1, New(2028, 2, 29)}, // leap year
		{New(2026, 11, 30), 3, New(2027, 2, 28)},
		{New(2026, 10, 1), 12, New(2027, 10, 1)},
		{New(2026, 12, 15), 1, New(2027, 1, 15)},
		{New(2026, 3, 31), -1, New(2026, 2, 28)},
		{New(2026, 1, 15), -2, New(2025, 11, 15)},
	}
	for _, c := range cases {
		if got := AddMonths(c.from, c.n); !got.Equal(c.want) {
			t.Errorf("AddMonths(%s, %d) = %s, want %s", c.from.Format(time.DateOnly), c.n, got.Format(time.DateOnly), c.want.Format(time.DateOnly))
		}
	}
}

func TestDays(t *testing.T) {
	if Days(New(2026, 2, 1), New(2026, 3, 1)) != 28 || Days(New(2028, 2, 1), New(2028, 3, 1)) != 29 || Days(New(2026, 3, 1), New(2026, 2, 1)) != -28 {
		t.Fatal("Days is wrong")
	}
}
