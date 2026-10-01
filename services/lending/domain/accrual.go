package domain

import (
	"time"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/platform/money"
)

// EntitledInterest returns the interest earned on this instalment at the
// start of business date asOf (that is, through the end of the previous day).
//
// Interest is earned evenly over the days from AccrualFrom to DueDate. The
// cumulative amount is floor(amount * elapsed / total), so it never exceeds
// the instalment's interest and reaches it exactly on the due date; no
// rounding residual accumulates however the days fall (28 to 31 day months,
// leap years, or accrual jobs that skipped a day).
func (i Instalment) EntitledInterest(asOf time.Time) int64 {
	if !asOf.Before(i.DueDate) {
		return i.InterestDue
	}
	total := bizdate.Days(i.AccrualFrom, i.DueDate)
	elapsed := bizdate.Days(i.AccrualFrom, asOf)
	if total <= 0 || elapsed <= 0 {
		return i.AccrualBase
	}
	part, err := money.MulDivFloor(i.InterestDue-i.AccrualBase, int64(elapsed), int64(total))
	if err != nil {
		// Unreachable: inputs are non-negative and total > 0.
		return i.AccrualBase
	}
	return i.AccrualBase + part
}

// AccrualLine is the interest to recognise for one instalment.
type AccrualLine struct {
	Seq        int
	Amount     int64 // to add to InterestAccrued
	NewAccrued int64
}

// AccrualFor returns the interest to recognise for business date d: for each
// instalment, the amount earned through the end of d that has not yet been
// recognised. Because it works from cumulative targets, a day the job missed
// is caught up automatically by the next run, and running it twice for the
// same day recognises nothing the second time.
func AccrualFor(insts []Instalment, d time.Time) (total int64, lines []AccrualLine) {
	asOf := d.AddDate(0, 0, 1)
	for _, i := range insts {
		target := i.EntitledInterest(asOf)
		if delta := target - i.InterestAccrued; delta > 0 {
			total += delta
			lines = append(lines, AccrualLine{Seq: i.Seq, Amount: delta, NewAccrued: target})
		}
	}
	return total, lines
}
