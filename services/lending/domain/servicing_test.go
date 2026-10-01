package domain

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"bankplatform.internal/platform/bizdate"
)

// plan example: 100,000.00 at 5% over 3 months from 1 Oct 2026.
// Due 1 Nov (31 days), 1 Dec (30 days), 1 Jan (31 days).
func example(t *testing.T) []Instalment {
	t.Helper()
	s, err := BuildSchedule(10_000_000, 500, 3, start)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func day(y int, m time.Month, d int) time.Time { return bizdate.New(y, m, d) }

func TestInterestIsEarnedEvenlyAndExactly(t *testing.T) {
	inst := example(t)[0] // 500,000 kobo over 31 days
	if got := inst.EntitledInterest(start); got != 0 {
		t.Fatalf("nothing is earned on the first morning, got %d", got)
	}
	if got := inst.EntitledInterest(day(2026, 10, 2)); got != 16_129 { // floor(500000*1/31)
		t.Fatalf("after one day got %d, want 16129", got)
	}
	if got := inst.EntitledInterest(day(2026, 11, 1)); got != 500_000 {
		t.Fatalf("on the due date got %d, want all 500000", got)
	}
	if got := inst.EntitledInterest(day(2027, 5, 1)); got != 500_000 {
		t.Fatalf("after the due date got %d: no extra interest accrues on an overdue instalment", got)
	}
	prev := int64(0)
	for d := start; !d.After(inst.DueDate); d = d.AddDate(0, 0, 1) {
		got := inst.EntitledInterest(d)
		if got < prev {
			t.Fatalf("entitled interest decreased on %s", d.Format(time.DateOnly))
		}
		prev = got
	}
}

// Daily accrual recognises exactly the scheduled interest by each due date,
// for periods of 28, 29, 30 and 31 days, with no rounding residual.
func TestDailyAccrualSumsExactlyForEveryMonthLength(t *testing.T) {
	for _, anchor := range []time.Time{day(2026, 1, 31), day(2028, 1, 31), day(2026, 10, 1), day(2026, 3, 15)} {
		insts, err := BuildSchedule(7_777_700, 437, 6, anchor)
		if err != nil {
			t.Fatal(err)
		}
		var recognised int64
		for d := anchor; d.Before(insts[len(insts)-1].DueDate.AddDate(0, 0, 5)); d = d.AddDate(0, 0, 1) {
			total, lines := AccrualFor(insts, d)
			for _, l := range lines {
				insts[l.Seq-1].InterestAccrued = l.NewAccrued
			}
			recognised += total
			// On each due date everything scheduled up to it is recognised.
			for _, i := range insts {
				if !d.AddDate(0, 0, 1).Before(i.DueDate) && i.InterestAccrued != i.InterestDue {
					t.Fatalf("anchor %s: instalment %d has %d recognised by %s, want %d", anchor.Format(time.DateOnly), i.Seq, i.InterestAccrued, d.Format(time.DateOnly), i.InterestDue)
				}
			}
		}
		if recognised != Totals(insts).Interest {
			t.Fatalf("anchor %s: recognised %d, scheduled %d", anchor.Format(time.DateOnly), recognised, Totals(insts).Interest)
		}
	}
}

func TestAccrualCatchesUpMissedDaysAndIsIdempotent(t *testing.T) {
	insts := example(t)
	apply := func(d time.Time) int64 {
		total, lines := AccrualFor(insts, d)
		for _, l := range lines {
			insts[l.Seq-1].InterestAccrued = l.NewAccrued
		}
		return total
	}
	// The job did not run for 1-9 October. Running it for the 10th recognises
	// all ten days at once.
	if got := apply(day(2026, 10, 10)); got != 161_290 { // floor(500000*10/31)
		t.Fatalf("catch-up recognised %d, want 161290", got)
	}
	// Running the same day again recognises nothing.
	if got := apply(day(2026, 10, 10)); got != 0 {
		t.Fatalf("second run for the same day recognised %d", got)
	}
	// An earlier date run late also recognises nothing more.
	if got := apply(day(2026, 10, 5)); got != 0 {
		t.Fatalf("late run for an earlier day recognised %d", got)
	}
}

func TestAllocateOnTimeInstalment(t *testing.T) {
	insts := example(t)
	a, err := Allocate(insts, 3_672_086, day(2026, 11, 1))
	if err != nil {
		t.Fatal(err)
	}
	if a.Applied != 3_672_086 || a.Unapplied != 0 || a.Interest != 500_000 || a.Principal != 3_172_086 || a.Settles {
		t.Fatalf("unexpected allocation %+v", a)
	}
	after, err := Apply(insts, a, day(2026, 11, 1))
	if err != nil || !after[0].FullyPaid() || after[1].PrincipalPaid != 0 {
		t.Fatalf("apply: %v %+v", err, after[0])
	}
}

func TestAllocatePartialPaysInterestBeforePrincipal(t *testing.T) {
	insts := example(t)
	a, err := Allocate(insts, 600_000, day(2026, 11, 1))
	if err != nil {
		t.Fatal(err)
	}
	if a.Interest != 500_000 || a.Principal != 100_000 || a.Unapplied != 0 {
		t.Fatalf("unexpected allocation %+v", a)
	}
	// Less than the interest due: all of it goes to interest.
	a, _ = Allocate(insts, 200_000, day(2026, 11, 1))
	if a.Interest != 200_000 || a.Principal != 0 {
		t.Fatalf("unexpected allocation %+v", a)
	}
}

// Two instalments overdue with a late fee: fees first, then interest oldest
// first across both, then principal oldest first.
func TestAllocateOverdueOrderIsFeesInterestPrincipalOldestFirst(t *testing.T) {
	insts := example(t)
	insts[0].FeesDue = 50_000
	today := day(2026, 12, 10) // instalments 1 and 2 are due and unpaid

	// Fees 50,000 + interest 500,000 + 341,396 = 891,396; then principal.
	a, err := Allocate(insts, 1_000_000, today)
	if err != nil {
		t.Fatal(err)
	}
	want := []AllocationLine{
		{Seq: 1, Fees: 50_000, Interest: 500_000, Principal: 108_604},
		{Seq: 2, Interest: 341_396},
	}
	if len(a.Lines) != 2 || a.Lines[0] != want[0] || a.Lines[1] != want[1] || a.Applied != 1_000_000 {
		t.Fatalf("got %+v", a.Lines)
	}
}

// Before the due date a payment pays ahead on the next instalment only:
// interest earned so far, then that instalment's principal. Anything beyond
// that is left with the customer.
func TestAllocatePayAheadIsLimitedToTheNextInstalment(t *testing.T) {
	insts := example(t)
	today := day(2026, 10, 11) // 10 of 31 days elapsed: 161,290 earned
	a, err := Allocate(insts, 5_000_000, today)
	if err != nil {
		t.Fatal(err)
	}
	if a.Interest != 161_290 || a.Principal != 3_172_086 || a.Applied != 3_333_376 || a.Unapplied != 1_666_624 || a.Settles {
		t.Fatalf("unexpected pay-ahead allocation %+v", a)
	}
	if len(a.Lines) != 1 || a.Lines[0].Seq != 1 {
		t.Fatalf("pay-ahead touched more than the next instalment: %+v", a.Lines)
	}
}

// Settling early pays all principal and only the interest earned so far.
func TestEarlySettlementWaivesUnearnedInterest(t *testing.T) {
	insts := example(t)
	today := day(2026, 10, 11)
	q := Payoff(insts, today)
	if q.Principal != 10_000_000 || q.Interest != 161_290 || q.Fees != 0 {
		t.Fatalf("payoff %+v", q)
	}
	a, err := Allocate(insts, 20_000_000, today)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Settles || !a.EarlySettlement || a.Applied != 10_161_290 || a.Unapplied != 9_838_710 {
		t.Fatalf("unexpected settlement %+v", a)
	}
	after, err := Apply(insts, a, today)
	if err != nil {
		t.Fatal(err)
	}
	if !AllPaid(after) || Totals(after).Principal != 10_000_000 || Totals(after).Interest != 161_290 {
		t.Fatalf("after settlement: %+v", after)
	}
	// What was earned is recognised at once by the next accrual run, and
	// nothing accrues after that.
	total, lines := AccrualFor(after, today)
	if total != 161_290 {
		t.Fatalf("the next accrual run must recognise the earned interest (161290), got %d", total)
	}
	for _, l := range lines {
		after[l.Seq-1].InterestAccrued = l.NewAccrued
	}
	if total, _ := AccrualFor(after, day(2027, 6, 30)); total != 0 {
		t.Fatalf("a settled loan accrued a further %d", total)
	}
	// Exactly the payoff amount also settles; one kobo less does not.
	if a, _ := Allocate(insts, 10_161_290, today); !a.Settles || a.Unapplied != 0 {
		t.Fatalf("exact payoff must settle: %+v", a)
	}
	if a, _ := Allocate(insts, 10_161_289, today); a.Settles {
		t.Fatal("one kobo short of the payoff must not settle")
	}
}

func TestFinalInstalmentSettlesWithoutBeingEarly(t *testing.T) {
	insts := example(t)
	// Pay instalments 1 and 2 on their due dates.
	for _, d := range []time.Time{day(2026, 11, 1), day(2026, 12, 1)} {
		a, err := Allocate(insts, 3_672_086, d)
		if err != nil {
			t.Fatal(err)
		}
		if insts, err = Apply(insts, a, d); err != nil {
			t.Fatal(err)
		}
	}
	a, err := Allocate(insts, 9_999_999, day(2027, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Settles || a.EarlySettlement || a.Applied != 3_672_085 {
		t.Fatalf("final instalment: %+v", a)
	}
	after, _ := Apply(insts, a, day(2027, 1, 1))
	if !AllPaid(after) {
		t.Fatal("loan should be fully paid")
	}
	if _, err := Allocate(after, 100, day(2027, 1, 2)); !errors.Is(err, ErrNothingToPay) {
		t.Fatalf("want ErrNothingToPay, got %v", err)
	}
}

func TestAllocateRejectsNonPositiveAmounts(t *testing.T) {
	for _, amt := range []int64{0, -1} {
		if _, err := Allocate(example(t), amt, start); !errors.Is(err, ErrInvalidTerms) {
			t.Errorf("amount %d: want ErrInvalidTerms, got %v", amt, err)
		}
	}
}

// Property: whatever sequence of payments arrives on whatever days, no
// instalment is ever overpaid, the customer is never charged more than was
// offered, principal paid never exceeds the loan, and interest paid never
// exceeds interest earned.
func TestRandomRepaymentSequencesPreserveInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 99))
	for range 2000 {
		principal := int64(1_000_000 + rng.IntN(20_000_000))
		n := 1 + rng.IntN(6)
		insts, err := BuildSchedule(principal, 100+rng.IntN(900), n, start)
		if err != nil {
			t.Fatal(err)
		}
		if rng.IntN(3) == 0 {
			insts[0].FeesDue = 50_000
		}
		today := start
		var paidTotal int64
		for step := 0; step < 40 && !AllPaid(insts); step++ {
			today = today.AddDate(0, 0, rng.IntN(25))
			amount := int64(1 + rng.IntN(int(principal)))
			a, err := Allocate(insts, amount, today)
			if errors.Is(err, ErrNothingToPay) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.Applied+a.Unapplied != amount || a.Applied <= 0 || a.Applied != a.Fees+a.Interest+a.Principal {
				t.Fatalf("allocation does not add up: %+v for amount %d", a, amount)
			}
			if insts, err = Apply(insts, a, today); err != nil {
				t.Fatalf("apply: %v", err)
			}
			paidTotal += a.Applied
			var principalPaid int64
			for _, i := range insts {
				if i.InterestPaid > i.EntitledInterest(today) {
					t.Fatalf("instalment %d: interest paid %d exceeds interest earned %d", i.Seq, i.InterestPaid, i.EntitledInterest(today))
				}
				principalPaid += i.PrincipalPaid
			}
			if principalPaid > principal || Totals(insts).Principal != principal {
				t.Fatalf("principal invariant broken: paid %d, scheduled %d, loan %d", principalPaid, Totals(insts).Principal, principal)
			}
			if a.Settles != AllPaid(insts) {
				t.Fatalf("Settles=%v but AllPaid=%v", a.Settles, AllPaid(insts))
			}
		}
		// The customer never pays more than principal + interest + fees.
		if paidTotal > principal+Totals(insts).Interest+Totals(insts).Fees {
			t.Fatalf("customer paid %d, more than principal+interest+fees", paidTotal)
		}
	}
}

func TestArrearsAndAmountDue(t *testing.T) {
	insts := example(t)
	if DaysPastDue(insts, day(2026, 11, 1)) != 0 {
		t.Fatal("an instalment due today is not past due")
	}
	if DaysPastDue(insts, day(2026, 11, 2)) != 1 || DaysPastDue(insts, day(2026, 12, 15)) != 44 {
		t.Fatal("days past due counts from the oldest unpaid due date")
	}
	if AmountDue(insts, day(2026, 10, 31)) != 0 || AmountDue(insts, day(2026, 11, 1)) != 3_672_086 || AmountDue(insts, day(2026, 12, 1)) != 7_344_172 {
		t.Fatal("amount due is wrong")
	}
	if got := LateFeeCandidates(insts, day(2026, 11, 4), 3); len(got) != 0 {
		t.Fatalf("within grace: got %d candidates", len(got))
	}
	if got := LateFeeCandidates(insts, day(2026, 11, 5), 3); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("after grace: got %+v", got)
	}
	prod := testProduct()
	for dpd, want := range map[int]string{0: "CURRENT", 1: "DPD_1_30", 30: "DPD_1_30", 31: "DPD_31_60", 90: "DPD_61_90", 91: "DPD_OVER_90", 400: "DPD_OVER_90"} {
		if got := prod.Bucket(dpd); got != want {
			t.Errorf("bucket(%d) = %s, want %s", dpd, got, want)
		}
	}
}

func TestRestructureKeepsPrincipalAndDoesNotEarnCarriedInterestTwice(t *testing.T) {
	insts := example(t)
	// Instalment 1 paid on time; instalment 2 partly paid; then restructure
	// on 20 December (instalment 2 overdue, instalment 3 nineteen days in).
	a, _ := Allocate(insts, 3_672_086, day(2026, 11, 1))
	insts, _ = Apply(insts, a, day(2026, 11, 1))
	a, _ = Allocate(insts, 400_000, day(2026, 12, 1)) // 341,396 interest + 58,604 principal
	insts, _ = Apply(insts, a, day(2026, 12, 1))
	// The accrual job has recognised everything through 19 December.
	for d := start; d.Before(day(2026, 12, 20)); d = d.AddDate(0, 0, 1) {
		_, lines := AccrualFor(insts, d)
		for _, l := range lines {
			insts[l.Seq-1].InterestAccrued = l.NewAccrued
		}
	}
	today := day(2026, 12, 20)
	recognisedBefore := insts[0].InterestAccrued + insts[1].InterestAccrued + insts[2].InterestAccrued
	earned3 := insts[2].EntitledInterest(today) // interest earned on instalment 3 so far, unpaid

	out, err := Restructure(insts, 500, 6, today)
	if err != nil {
		t.Fatal(err)
	}
	if Totals(out).Principal != 10_000_000 {
		t.Fatalf("principal column sums to %d, want the original 10,000,000", Totals(out).Principal)
	}
	outstanding := int64(10_000_000 - 3_172_086 - 58_604)
	var newPrincipal, recognisedAfter int64
	for _, i := range out {
		recognisedAfter += i.InterestAccrued
		if !i.FullyPaid() {
			newPrincipal += i.PrincipalOutstanding()
		}
	}
	if newPrincipal != outstanding {
		t.Fatalf("outstanding principal %d, want %d", newPrincipal, outstanding)
	}
	if recognisedAfter != recognisedBefore {
		t.Fatalf("recognised interest changed from %d to %d across the restructure", recognisedBefore, recognisedAfter)
	}
	// Kept: instalment 1 (paid) and the closed part of instalment 2; then six new ones.
	if len(out) != 8 || !out[0].FullyPaid() || !out[1].FullyPaid() || out[1].PrincipalDue != 58_604 {
		t.Fatalf("unexpected restructured schedule: %+v", out)
	}
	first := out[2]
	if first.AccrualBase != earned3 || !first.AccrualFrom.Equal(today) || first.EntitledInterest(today) != earned3 {
		t.Fatalf("carried interest must be already earned on day one: %+v (earned %d)", first, earned3)
	}
	fresh, _ := BuildSchedule(outstanding, 500, 6, today)
	if first.InterestDue != fresh[0].InterestDue+earned3 {
		t.Fatalf("first new instalment interest %d, want new period interest %d + carried %d", first.InterestDue, fresh[0].InterestDue, earned3)
	}
	if DaysPastDue(out, today) != 0 {
		t.Fatal("a restructured schedule starts with nothing past due")
	}
	for i, inst := range out {
		if inst.Seq != i+1 {
			t.Fatalf("sequence numbers must be contiguous, got %d at %d", inst.Seq, i)
		}
	}
}
