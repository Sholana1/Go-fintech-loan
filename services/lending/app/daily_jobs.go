package app

import (
	"context"
	"fmt"
	"time"
)

// Daily servicing jobs.
//
// Scheduling approach: nothing here depends on a timer firing at the right
// moment. Each job asks the database "which business dates have not been
// done?" and does them in order. A service that was down for three days
// simply finds three dates outstanding when it returns. Completion is
// recorded per date (accrual_runs, job_runs), and every per-loan step is
// idempotent, so running a job twice, or from two instances at once, has the
// same effect as running it once.

const (
	jobArrears = "ARREARS"
	// accrualLockBase namespaces the advisory locks used by the accrual job;
	// the business date (days since the Unix epoch) is added to it.
	accrualLockBase int64 = 7_100_000_000
)

// RunDailyJobs brings interest accrual up to yesterday and classifies
// arrears for today. It is called every minute; when nothing is outstanding
// it does two cheap reads and returns.
func (s *Service) RunDailyJobs(ctx context.Context) error {
	today := s.today()

	from := today.AddDate(0, 0, -1)
	if latest, ok, err := s.store.LatestAccrualDate(ctx); err != nil {
		return err
	} else if ok {
		from = latest.AddDate(0, 0, 1)
	}
	for d := from; d.Before(today); d = d.AddDate(0, 0, 1) {
		if err := s.AccrueInterest(ctx, d); err != nil {
			return fmt.Errorf("accrue interest for %s: %w", d.Format(time.DateOnly), err)
		}
	}

	done, err := s.store.JobCompleted(ctx, jobArrears, today)
	if err != nil {
		return err
	}
	if !done {
		complete, err := s.ClassifyArrears(ctx, today)
		if err != nil {
			return fmt.Errorf("classify arrears for %s: %w", today.Format(time.DateOnly), err)
		}
		if complete {
			if err := s.store.RecordJobRun(ctx, jobArrears, today, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
