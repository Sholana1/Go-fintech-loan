package app

import (
	"errors"
	"time"
)

// Config holds lending's operational tuning. None of it changes what a
// customer pays; product and policy configuration do that.
type Config struct {
	// ConsentPolicyVersion identifies the consent wording the customer
	// agreed to when submitting an application.
	ConsentPolicyVersion string

	// Per-dependency deadlines for assessment (plan section 8.11).
	IdentityTimeout time.Duration
	FraudTimeout    time.Duration
	BureauTimeout   time.Duration
	LedgerTimeout   time.Duration
	PayoutTimeout   time.Duration

	// AssessmentLease is how long one worker owns an application before
	// another may pick it up (it must exceed the sum of the deadlines above).
	AssessmentLease time.Duration
	// RetryBackoff is the wait before the 1st, 2nd, ... retry of deferred
	// work; the last value repeats.
	RetryBackoff []time.Duration
	// InlineGrace is how long the sweeper leaves a new intent to the request
	// that created it before picking it up.
	InlineGrace time.Duration

	IdempotencyRetention time.Duration

	// PayoutLease is how long one worker owns a payout while calling the
	// provider. PayoutQueryBackoff spaces status queries for unknown outcomes.
	PayoutLease        time.Duration
	PayoutQueryBackoff []time.Duration
	// UnresolvedAlertAfter opens an operations exception for a payout whose
	// outcome has been unknown this long.
	UnresolvedAlertAfter time.Duration
	// AbsentFromReportMeansFailed lets reconciliation treat a transfer that
	// is missing from the provider's settlement report for a closed business
	// date as failed. It must only be enabled for a provider whose contract
	// states that the report is complete and final.
	AbsentFromReportMeansFailed bool

	// External repayments. ExternalPaymentValidity is how long a payment
	// reference may be paid before we stop polling for it (a payment proven
	// later is still credited). ExternalPaymentPollBackoff spaces status
	// queries while the customer is paying; a webhook cuts the wait short.
	ExternalPaymentValidity    time.Duration
	ExternalPaymentPollBackoff []time.Duration
	ExternalPaymentLease       time.Duration
	CollectionTimeout          time.Duration
}

// DefaultConfig returns production-shaped defaults.
func DefaultConfig() Config {
	return Config{
		ConsentPolicyVersion: "consent-v1",
		IdentityTimeout:      3 * time.Second,
		FraudTimeout:         3 * time.Second,
		BureauTimeout:        30 * time.Second,
		LedgerTimeout:        5 * time.Second,
		PayoutTimeout:        20 * time.Second,
		AssessmentLease:      90 * time.Second,
		RetryBackoff:         []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute},
		InlineGrace:          5 * time.Second,
		IdempotencyRetention: 24 * time.Hour,
		PayoutLease:          time.Minute,
		PayoutQueryBackoff:   []time.Duration{5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute},
		UnresolvedAlertAfter: 30 * time.Minute,

		ExternalPaymentValidity:    30 * time.Minute,
		ExternalPaymentPollBackoff: []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute},
		ExternalPaymentLease:       time.Minute,
		CollectionTimeout:          15 * time.Second,
	}
}

func (c Config) validate() error {
	if c.ConsentPolicyVersion == "" || c.IdentityTimeout <= 0 || c.FraudTimeout <= 0 || c.BureauTimeout <= 0 ||
		c.LedgerTimeout <= 0 || c.PayoutTimeout <= 0 || c.AssessmentLease <= 0 || len(c.RetryBackoff) == 0 ||
		c.IdempotencyRetention <= 0 || c.PayoutLease <= 0 || len(c.PayoutQueryBackoff) == 0 || c.UnresolvedAlertAfter <= 0 ||
		c.ExternalPaymentValidity <= 0 || len(c.ExternalPaymentPollBackoff) == 0 || c.ExternalPaymentLease <= 0 || c.CollectionTimeout <= 0 {
		return errors.New("lending: incomplete configuration")
	}
	if c.AssessmentLease < c.IdentityTimeout+c.FraudTimeout+c.BureauTimeout {
		return errors.New("lending: assessment lease must cover the dependency deadlines")
	}
	if c.PayoutLease < c.PayoutTimeout+c.LedgerTimeout {
		return errors.New("lending: payout lease must cover the provider and ledger deadlines")
	}
	if c.ExternalPaymentLease < c.CollectionTimeout+2*c.LedgerTimeout {
		return errors.New("lending: external payment lease must cover the provider and ledger deadlines")
	}
	return nil
}
