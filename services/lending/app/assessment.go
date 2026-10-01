package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// RunAssessmentWorkers starts n workers that assess applications as they
// are submitted. Concurrency is bounded at n; it blocks until ctx is done.
func (s *Service) RunAssessmentWorkers(ctx context.Context, n int) error {
	done := make(chan struct{})
	for range n {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-s.kick:
					if err := s.Assess(ctx, id); err != nil && ctx.Err() == nil {
						s.log.ErrorContext(ctx, "assessment failed; the sweeper will retry", "application_id", id.String(), "error", err.Error())
					}
				}
			}
		}()
	}
	for range n {
		<-done
	}
	return nil
}

// AssessDue assesses every application whose attempt is due. It is the
// recovery path: applications left behind by a crash, a full queue, or a
// dependency outage are picked up here.
func (s *Service) AssessDue(ctx context.Context) error {
	ids, err := s.store.DueApplications(ctx, s.now(), 50)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := s.Assess(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("application %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Assess runs the checks for one application and records the decision (T1).
//
// It is safe to call concurrently and repeatedly: the lease taken by
// ClaimApplication admits one worker at a time, every provider call carries
// a reference derived from the application id (so a repeat is not a second
// enquiry), and the final write is a compare-and-set on the state.
//
// If a dependency is unavailable the application is NOT declined and NOT
// approved on partial information. It stays in ASSESSING with waiting_on set
// and is retried with backoff until it expires.
func (s *Service) Assess(ctx context.Context, applicationID uuid.UUID) error {
	now := s.now()
	a, ok, err := s.store.ClaimApplication(ctx, applicationID, now, s.cfg.AssessmentLease)
	if err != nil || !ok {
		return err
	}
	if !now.Before(a.ExpiresAt) {
		return s.expireApplication(ctx, a, now)
	}

	// --- Identity (KYC status comes from the system of record, not the request).
	customer, err := timed(s, ctx, "identity", s.cfg.IdentityTimeout, func(ctx context.Context) (Customer, error) {
		return s.identity.GetCustomer(ctx, a.CustomerID)
	})
	if err != nil {
		return s.deferAssessment(ctx, a, "IDENTITY", err)
	}

	// --- Internal exposure and velocity.
	exposure, err := s.store.CustomerExposure(ctx, a.CustomerID)
	if err != nil {
		return s.deferAssessment(ctx, a, "DATABASE", err)
	}
	recent, err := s.store.CountApplicationsSince(ctx, a.CustomerID, now.Add(-24*time.Hour))
	if err != nil {
		return s.deferAssessment(ctx, a, "DATABASE", err)
	}

	// --- Fraud screen.
	fraud, err := timed(s, ctx, "fraud", s.cfg.FraudTimeout, func(ctx context.Context) (FraudResult, error) {
		return s.fraud.Screen(ctx, FraudInput{CustomerID: a.CustomerID, ApplicationID: a.ID, RequestedMinor: a.RequestedPrincipalMinor,
			Applications24h: recent, HasPriorWriteOff: exposure.HasPriorWriteOff})
	})
	if err != nil {
		return s.deferAssessment(ctx, a, "FRAUD_SCREEN", err)
	}

	features := domain.Features{
		KYCVerified: customer.KYCVerified, KYCTier: customer.KYCTier,
		// A customer without a ledger account cannot be disbursed to.
		CustomerActive: customer.Active && customer.DepositAccountCode != "",
		AgeYears:       bizdate.Age(customer.DateOfBirth, s.today()),
		FraudDecision:  fraud.Decision, FraudSignals: fraud.Signals,
		StatedMonthlyIncomeMinor:          a.StatedMonthlyIncomeMinor,
		InternalOutstandingPrincipalMinor: exposure.OutstandingPrincipalMinor,
		HasActiveLoan:                     exposure.HasActiveLoan,
		HasPriorWriteOff:                  exposure.HasPriorWriteOff,
		RequestedPrincipalMinor:           a.RequestedPrincipalMinor,
		TenorMonths:                       a.TenorMonths,
	}
	refs := map[string]string{"fraud_screen": "rules-v1"}

	// An application that is already ruled out is declined without a bureau
	// enquiry: the enquiry would cost money and mark the customer's file for
	// a loan we will not make.
	if reasons := domain.Ineligible(s.product, features); len(reasons) > 0 {
		return s.recordDecision(ctx, a, features, refs, domain.Decision{Outcome: domain.OutcomeDecline, ReasonCodes: reasons})
	}

	// --- Credit bureau. The report is a legal precondition for lending; if
	// it cannot be obtained the application waits.
	subject, err := timed(s, ctx, "identity", s.cfg.IdentityTimeout, func(ctx context.Context) (BureauSubject, error) {
		return s.identity.BureauSubject(ctx, a.CustomerID, a.ConsentRef)
	})
	if err != nil {
		return s.deferAssessment(ctx, a, "IDENTITY", err)
	}
	report, err := timed(s, ctx, "bureau", s.cfg.BureauTimeout, func(ctx context.Context) (domain.BureauReport, error) {
		// The request reference is the application id: a retry of this
		// assessment repeats the same enquiry rather than making a new one.
		return s.bureau.FetchReport(ctx, a.ID.String(), subject)
	})
	if err != nil {
		return s.deferAssessment(ctx, a, "CREDIT_BUREAU", err)
	}
	sum := sha256.Sum256(report.Raw)
	if err := s.store.InsertBureauReport(ctx, a.ID, a.CustomerID, report, hex.EncodeToString(sum[:])); err != nil {
		return s.deferAssessment(ctx, a, "DATABASE", err)
	}
	refs["bureau_report"] = report.ID.String()
	refs["bureau_provider"] = report.Provider

	features.BureauScore = report.Score
	features.BureauWorstDelinquencyDays12m = report.WorstDelinquencyDays12m
	features.BureauMonthlyObligationsMinor = report.MonthlyObligationsMinor

	stageStart := time.Now()
	decision := domain.Evaluate(s.policy, s.product, features)
	s.metrics.StageDuration("policy", time.Since(stageStart), true)
	return s.recordDecision(ctx, a, features, refs, decision)
}

// timed runs one dependency call under its deadline and records its duration.
func timed[T any](s *Service, ctx context.Context, stage string, timeout time.Duration, call func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	out, err := call(ctx)
	s.metrics.StageDuration(stage, time.Since(start), err == nil)
	return out, err
}

// deferAssessment leaves the application in ASSESSING, records what it is
// waiting for, and schedules the next attempt.
func (s *Service) deferAssessment(ctx context.Context, a domain.Application, waitingOn string, cause error) error {
	now := s.now()
	next := now.Add(backoff(s.cfg.RetryBackoff, a.Attempts))
	s.log.WarnContext(ctx, "assessment deferred", "application_id", a.ID.String(), "waiting_on", waitingOn, "attempt", a.Attempts, "error", cause.Error())
	// Use a fresh context: the request context may be the thing that expired.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.store.TransitionApplication(writeCtx, a.ID, []domain.ApplicationState{domain.AppAssessing},
		postgres.ApplicationUpdate{To: domain.AppAssessing, WaitingOn: waitingOn, NextAttemptAt: &next}, now)
	return err
}
