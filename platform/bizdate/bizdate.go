// Package bizdate defines the platform's business calendar: dates are
// calendar days in Africa/Lagos, stored and compared as UTC midnight.
//
// Every service that needs "today" for money (posting dates, due dates,
// accrual dates) uses this package, so a loan due "on the 5th" and a journal
// posted "on the 5th" mean the same day.
package bizdate

import "time"

// lagos is UTC+1 all year: Nigeria observes no daylight saving time. A fixed
// zone is exact and does not depend on tzdata being present in the image.
var lagos = time.FixedZone("Africa/Lagos", 3600)

// Of returns the business date on which instant t falls, as UTC midnight.
func Of(t time.Time) time.Time {
	y, m, d := t.In(lagos).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// New builds a business date.
func New(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// StartOf returns the instant at which business date d begins in Lagos.
func StartOf(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, lagos)
}

// Days returns the whole number of days from a to b (negative if b < a).
// Both must be business dates.
func Days(a, b time.Time) int {
	return int(b.Sub(a).Hours() / 24)
}

// AddMonths returns the date n months after d, keeping the day of month
// where it exists and otherwise using the last day of the target month
// (31 January + 1 month = 28 or 29 February).
//
// It always computes from the original date, so a schedule anchored on the
// 31st falls on 31 March after falling on 28 February.
func AddMonths(d time.Time, n int) time.Time {
	y, m := d.Year(), int(d.Month())-1+n
	y += m / 12
	m %= 12
	if m < 0 {
		m += 12
		y--
	}
	month := time.Month(m + 1)
	day := d.Day()
	if last := daysIn(y, month); day > last {
		day = last
	}
	return New(y, month, day)
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// Age returns the number of completed years from dateOfBirth to on.
func Age(dateOfBirth, on time.Time) int {
	years := on.Year() - dateOfBirth.Year()
	if on.Month() < dateOfBirth.Month() || (on.Month() == dateOfBirth.Month() && on.Day() < dateOfBirth.Day()) {
		years--
	}
	return years
}
