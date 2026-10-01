package domain

import (
	"fmt"
	"time"
)

// Restructure replaces the unpaid part of a schedule with newTenor fresh
// monthly instalments starting today, at the same rate.
//
//   - Fully paid instalments are kept as they are.
//   - A partly paid instalment is closed at what was paid.
//   - Outstanding principal is re-amortised over the new tenor.
//   - Interest already earned but unpaid, and unpaid fees, are carried onto
//     the first new instalment: they remain owed, and because the interest is
//     already earned it is carried in AccrualBase so it is not earned twice.
//
// The principal column of the result still sums to the original loan.
func Restructure(insts []Instalment, monthlyRateBps, newTenor int, today time.Time) ([]Instalment, error) {
	var (
		kept                         []Instalment
		outstanding                  int64
		carriedInterest, carriedFees int64
		carriedAccrued               int64
	)
	for _, i := range insts {
		if i.FullyPaid() {
			kept = append(kept, i)
			continue
		}
		earned := i.EntitledInterest(today)
		outstanding += i.PrincipalOutstanding()
		if earned > i.InterestPaid {
			carriedInterest += earned - i.InterestPaid
		}
		carriedFees += i.FeesOutstanding()
		// Interest already recognised in the ledger stays recognised: the part
		// covered by payments remains on the closed instalment and the rest
		// moves with the carried interest, so the receivable stays matched.
		recognisedPaid := min(i.InterestAccrued, i.InterestPaid)
		carriedAccrued += i.InterestAccrued - recognisedPaid
		if i.PrincipalPaid+i.InterestPaid+i.FeesPaid > 0 {
			closed := i
			closed.PrincipalDue, closed.InterestDue, closed.FeesDue = i.PrincipalPaid, i.InterestPaid, i.FeesPaid
			closed.InterestAccrued = recognisedPaid
			closed.AccrualBase = min(closed.AccrualBase, closed.InterestDue)
			if closed.DueDate.After(today) {
				// The closed part is complete today; nothing further is
				// earned on it, and any recognition lag is caught up by the
				// next accrual run.
				closed.DueDate = today
			}
			kept = append(kept, closed)
		}
	}
	if outstanding <= 0 {
		return nil, fmt.Errorf("%w: no outstanding principal to restructure", ErrInvalidTerms)
	}
	fresh, err := BuildSchedule(outstanding, monthlyRateBps, newTenor, today)
	if err != nil {
		return nil, err
	}
	fresh[0].InterestDue += carriedInterest
	fresh[0].AccrualBase = carriedInterest
	fresh[0].InterestAccrued = carriedAccrued
	fresh[0].FeesDue = carriedFees

	// Renumber so sequence numbers stay unique and ordered within the version.
	out := make([]Instalment, 0, len(kept)+len(fresh))
	for _, i := range kept {
		i.Seq = len(out) + 1
		out = append(out, i)
	}
	for _, i := range fresh {
		i.Seq = len(out) + 1
		out = append(out, i)
	}
	return out, nil
}
