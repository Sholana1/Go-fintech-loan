package domain

import (
	"time"

	"bankplatform.internal/platform/bizdate"
)

// AmountDue returns what is payable now without paying ahead: outstanding
// fees plus interest and principal on instalments due on or before today.
// The collections job debits at most this amount.
func AmountDue(insts []Instalment, today time.Time) int64 {
	var due int64
	for _, i := range insts {
		due += i.FeesOutstanding()
		if !i.DueDate.After(today) {
			due += (i.InterestDue - i.InterestPaid) + i.PrincipalOutstanding()
		}
	}
	return due
}

// DaysPastDue returns how many days the oldest unpaid due instalment is
// overdue as of today. An instalment due today is not yet past due.
func DaysPastDue(insts []Instalment, today time.Time) int {
	for _, i := range insts { // schedule order: the first hit is the oldest
		if i.DueDate.Before(today) && !i.FullyPaid() {
			return bizdate.Days(i.DueDate, today)
		}
	}
	return 0
}

// LateFeeCandidates returns the instalments that have been unpaid for at
// least graceDays and therefore attract the product's late fee (once each;
// the caller's unique constraint prevents a second assessment).
func LateFeeCandidates(insts []Instalment, today time.Time, graceDays int) []Instalment {
	var out []Instalment
	for _, i := range insts {
		scheduledUnpaid := i.PrincipalOutstanding() > 0 || i.InterestPaid < i.InterestDue
		if scheduledUnpaid && bizdate.Days(i.DueDate, today) > graceDays {
			out = append(out, i)
		}
	}
	return out
}
