package domain

import (
	"errors"
	"fmt"
	"time"
)

// PayoffQuote is what it costs to settle the loan in full on a given date.
// Interest not yet earned is not charged.
type PayoffQuote struct {
	Fees      int64
	Interest  int64
	Principal int64
}

func (q PayoffQuote) Total() int64 { return q.Fees + q.Interest + q.Principal }

// Payoff computes the settlement amount as of business date today.
func Payoff(insts []Instalment, today time.Time) PayoffQuote {
	var q PayoffQuote
	for _, i := range insts {
		q.Fees += i.FeesOutstanding()
		q.Principal += i.PrincipalOutstanding()
		if owed := i.EntitledInterest(today) - i.InterestPaid; owed > 0 {
			q.Interest += owed
		}
	}
	return q
}

// AllocationLine is what one repayment pays against one instalment.
type AllocationLine struct {
	Seq       int   `json:"seq"`
	Fees      int64 `json:"fees"`
	Interest  int64 `json:"interest"`
	Principal int64 `json:"principal"`
}

// Allocation is the result of applying a repayment to a schedule.
type Allocation struct {
	Lines []AllocationLine `json:"lines"`
	// Applied is the amount actually taken from the customer; Unapplied is
	// the part of the offered amount that has nothing to pay and is left in
	// the customer's account.
	Applied   int64 `json:"applied"`
	Unapplied int64 `json:"unapplied"`
	Fees      int64 `json:"fees"`
	Interest  int64 `json:"interest"`
	Principal int64 `json:"principal"`
	// Settles is true when this payment repays the loan in full.
	Settles bool `json:"settles"`
	// EarlySettlement is true when the loan is settled before its final due
	// date, so interest not yet earned is waived and the schedule is rewritten.
	EarlySettlement bool `json:"early_settlement"`
}

var ErrNothingToPay = errors.New("nothing is payable on this loan")

// Allocate applies amount to the schedule as of business date today.
//
// Product rules (see docs/loans/PRODUCT_RULES.md):
//
//  1. If the amount covers the payoff figure, the loan is settled: all fees,
//     all earned interest and all principal are paid; unearned interest is
//     waived; anything above the payoff is not taken.
//  2. Otherwise amounts already due are paid first, in this order across all
//     due instalments, oldest first within each: fees, then interest, then
//     principal.
//  3. What remains may pay ahead on the next instalment only: its interest
//     earned to date, then its principal.
//  4. Anything still remaining is not taken. Partial prepayment of later
//     instalments is not offered by this product version.
func Allocate(insts []Instalment, amount int64, today time.Time) (Allocation, error) {
	if amount <= 0 {
		return Allocation{}, fmt.Errorf("%w: amount must be positive", ErrInvalidTerms)
	}
	payoff := Payoff(insts, today)
	if payoff.Total() == 0 {
		return Allocation{}, ErrNothingToPay
	}

	lines := make(map[int]*AllocationLine, len(insts))
	line := func(seq int) *AllocationLine {
		if lines[seq] == nil {
			lines[seq] = &AllocationLine{Seq: seq}
		}
		return lines[seq]
	}
	var a Allocation

	if amount >= payoff.Total() {
		for _, i := range insts {
			l := AllocationLine{Seq: i.Seq, Fees: i.FeesOutstanding(), Principal: i.PrincipalOutstanding()}
			if owed := i.EntitledInterest(today) - i.InterestPaid; owed > 0 {
				l.Interest = owed
			}
			if i.EntitledInterest(today) < i.InterestDue {
				a.EarlySettlement = true
			}
			if l.Fees+l.Interest+l.Principal > 0 {
				*line(i.Seq) = l
			}
		}
		a.Settles = true
		a.Unapplied = amount - payoff.Total()
		return finish(a, lines, insts), nil
	}

	remaining := amount
	take := func(want int64) int64 {
		if want <= 0 || remaining <= 0 {
			return 0
		}
		got := min(want, remaining)
		remaining -= got
		return got
	}
	isDue := func(i Instalment) bool { return !i.DueDate.After(today) }

	// Tier 1: fees. Fees exist only on instalments that are already overdue.
	for _, i := range insts {
		line(i.Seq).Fees += take(i.FeesOutstanding())
	}
	// Tier 2: interest on due instalments, oldest first.
	for _, i := range insts {
		if isDue(i) {
			line(i.Seq).Interest += take(i.InterestDue - i.InterestPaid)
		}
	}
	// Tier 3: principal on due instalments, oldest first.
	for _, i := range insts {
		if isDue(i) {
			line(i.Seq).Principal += take(i.PrincipalOutstanding())
		}
	}
	// Tier 4: pay ahead on the next instalment only.
	for _, i := range insts {
		if isDue(i) {
			continue
		}
		line(i.Seq).Interest += take(i.EntitledInterest(today) - i.InterestPaid)
		line(i.Seq).Principal += take(i.PrincipalOutstanding())
		break
	}

	a.Unapplied = remaining
	a = finish(a, lines, insts)
	if a.Applied == 0 {
		return Allocation{}, ErrNothingToPay
	}
	return a, nil
}

func finish(a Allocation, lines map[int]*AllocationLine, insts []Instalment) Allocation {
	for _, i := range insts { // schedule order
		l := lines[i.Seq]
		if l == nil || l.Fees+l.Interest+l.Principal == 0 {
			continue
		}
		a.Lines = append(a.Lines, *l)
		a.Fees += l.Fees
		a.Interest += l.Interest
		a.Principal += l.Principal
	}
	a.Applied = a.Fees + a.Interest + a.Principal
	return a
}

// Apply returns the schedule after the allocation has been paid.
//
// For an early settlement the unearned interest is removed from the schedule
// (InterestDue becomes what was earned), so that every instalment ends fully
// paid and the daily accrual job has nothing further to recognise.
func Apply(insts []Instalment, a Allocation, today time.Time) ([]Instalment, error) {
	bySeq := make(map[int]AllocationLine, len(a.Lines))
	for _, l := range a.Lines {
		bySeq[l.Seq] = l
	}
	out := make([]Instalment, len(insts))
	for idx, i := range insts {
		l := bySeq[i.Seq]
		if a.Settles {
			// Freeze the instalment at what has been earned: that amount is
			// now the whole of its interest and is fully earned today, so
			// nothing more accrues and the accrual job recognises any
			// remaining lag on its next run.
			earned := i.EntitledInterest(today)
			if earned < i.InterestDue {
				i.InterestDue = earned
				i.AccrualBase = earned
				i.AccrualFrom = today
			}
		}
		i.FeesPaid += l.Fees
		i.InterestPaid += l.Interest
		i.PrincipalPaid += l.Principal
		if i.FeesPaid > i.FeesDue || i.InterestPaid > i.InterestDue || i.PrincipalPaid > i.PrincipalDue {
			return nil, fmt.Errorf("allocation overpays instalment %d", i.Seq)
		}
		out[idx] = i
	}
	if a.Settles {
		for _, i := range out {
			if !i.FullyPaid() {
				return nil, fmt.Errorf("settlement leaves instalment %d unpaid", i.Seq)
			}
		}
	}
	return out, nil
}

// AllPaid reports whether every instalment is fully paid.
func AllPaid(insts []Instalment) bool {
	for _, i := range insts {
		if !i.FullyPaid() {
			return false
		}
	}
	return true
}
