package domain

import (
	"time"

	"github.com/google/uuid"
)

// ExternalPaymentState is the lifecycle of a repayment made from outside the
// bank through a payment provider.
//
//	INITIATED  we issued a reference; waiting for the provider to confirm
//	VERIFIED   the provider confirmed the payment; the ledger credit is due
//	CREDITED   the customer's deposit account was credited; the repayment
//	           from that account is due
//	APPLIED    the repayment was allocated to the loan
//	UNAPPLIED  the money stays in the customer's account (the loan no longer
//	           needs it, or the repayment was refused)
//	FAILED     the provider says the payment failed
//	EXPIRED    nothing was paid within the validity window
//	REVIEW     the provider confirmed something we cannot credit
//	           automatically (wrong currency); a person decides
type ExternalPaymentState string

const (
	ExternalInitiated ExternalPaymentState = "INITIATED"
	ExternalVerified  ExternalPaymentState = "VERIFIED"
	ExternalCredited  ExternalPaymentState = "CREDITED"
	ExternalApplied   ExternalPaymentState = "APPLIED"
	ExternalUnapplied ExternalPaymentState = "UNAPPLIED"
	ExternalFailed    ExternalPaymentState = "FAILED"
	ExternalExpired   ExternalPaymentState = "EXPIRED"
	ExternalReview    ExternalPaymentState = "REVIEW"
)

var externalPaymentTransitions = map[ExternalPaymentState][]ExternalPaymentState{
	ExternalInitiated: {ExternalVerified, ExternalFailed, ExternalExpired, ExternalReview},
	ExternalVerified:  {ExternalCredited},
	ExternalCredited:  {ExternalApplied, ExternalUnapplied},
	// Money that really arrived is always recognised, however late: a
	// payment we gave up on becomes VERIFIED when the provider proves it.
	// The reverse never happens: nothing moves a confirmed payment back.
	ExternalFailed:  {ExternalVerified, ExternalReview},
	ExternalExpired: {ExternalVerified, ExternalReview},
}

func (from ExternalPaymentState) CanTransition(to ExternalPaymentState) bool {
	for _, s := range externalPaymentTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// AwaitingProvider reports whether the provider's answer can still change
// what we do with the payment.
func (s ExternalPaymentState) AwaitingProvider() bool {
	return s == ExternalInitiated || s == ExternalFailed || s == ExternalExpired
}

// SourceExternal marks a repayment funded by an external payment.
const SourceExternal = "EXTERNAL"

// ExternalPayment is one payment a customer makes towards a loan through a
// payment provider.
type ExternalPayment struct {
	ID         uuid.UUID
	LoanID     uuid.UUID
	CustomerID uuid.UUID
	// Reference is generated and stored before the customer pays. The
	// provider's checkout, its webhook and our status query all carry it.
	Reference   string
	Provider    string
	ProviderRef string
	// ExpectedMinor is what the customer said they would pay; VerifiedMinor
	// is what the provider confirmed. Only the latter is ever credited.
	ExpectedMinor int64
	VerifiedMinor *int64
	Currency      string
	State         ExternalPaymentState
	LastCode      string
	JournalID     *int64
	RepaymentID   *uuid.UUID
	Attempts      int
	ExpiresAt     time.Time
	StateChanged  time.Time
	CreatedAt     time.Time
}

// Exception kinds raised for external payments.
const (
	// A webhook named a payment reference we never issued.
	ExceptionUnknownPayment = "EXTERNAL_PAYMENT_UNKNOWN_REFERENCE"
	// The provider confirmed a payment in a currency we cannot credit.
	ExceptionPaymentCurrency = "EXTERNAL_PAYMENT_CURRENCY_MISMATCH"
	// We could not get an answer from the provider about a payment for
	// longer than its validity: the customer may have paid and not been
	// credited.
	ExceptionPaymentUnresolved = "EXTERNAL_PAYMENT_UNRESOLVED"
)
