// Package app implements the personal-loan use cases.
//
// Structure:
//
//	submission.go, assessment.go, decision.go, expiry.go
//	                     the application: submit, assess, decide, offer, expire
//	acceptance.go, destination.go
//	                     accept an offer: book the loan and its disbursement
//	posting.go, disbursement.go, fees.go, writeoff.go, recovery.go
//	                     the posting-intent driver (lending -> ledger,
//	                     crash-safe) and what each kind of posting applies
//	repayments.go, loan_views.go
//	                     repayments from the customer's account; loan views
//	external_repayment.go, external_payment_driver.go, external_payment_webhook.go
//	                     repayments from outside the bank, verified with the
//	                     payment provider before anything is credited
//	daily_jobs.go, accrual.go, arrears.go, collections.go
//	                     interest accrual, arrears, late fees, auto-debit
//	payout_driver.go, payout_outcome.go, payout_callback.go
//	                     external payout state machine and callbacks
//	recon_ledger.go, recon_payouts.go
//	                     reconciliation against the ledger and the provider
//	manual_review.go, admin_actions.go, ops_reports.go
//	                     staff: manual review, maker-checker actions, reports
//	events.go            Kafka consumer handler
//
// Dependencies on other systems are the small interfaces in this file. Each
// has a real adapter (gRPC or HTTP) and, for providers we have no verified
// production contract with, a simulator client. Persistence is the concrete
// postgres.Store: there is exactly one implementation and tests run against
// real PostgreSQL, so an interface would add indirection without adding a
// seam anyone uses.
//
// The rule that shapes every use case: a database transaction never spans a
// network call. Work that needs both is split into "record intent, commit",
// "call", "record outcome, commit", and each step is idempotent so a crash
// between any two is repaired by running the step again.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
)

// Clock supplies the current instant. Tests control it.
type Clock interface {
	Now() time.Time
}

// Customer is the KYC view of a customer, from the identity service.
type Customer struct {
	ID                 uuid.UUID
	Active             bool
	KYCVerified        bool
	KYCTier            int
	FullName           string
	DateOfBirth        time.Time
	DepositAccountCode string
}

// BureauSubject identifies a person to a credit bureau.
type BureauSubject struct {
	BVN         string
	FullName    string
	DateOfBirth time.Time
}

// Identity is what lending needs from the identity service.
type Identity interface {
	GetCustomer(ctx context.Context, id uuid.UUID) (Customer, error)
	// VerifyPIN is the step-up check at acceptance.
	VerifyPIN(ctx context.Context, id uuid.UUID, pin string) (verified, locked bool, err error)
	// BureauSubject releases the BVN for a consented bureau enquiry.
	BureauSubject(ctx context.Context, id uuid.UUID, consentRef uuid.UUID) (BureauSubject, error)
}

// Ledger errors lending reacts to. Anything else from the ledger is treated
// as transient (the call is repeated with the same reference) unless it is
// ErrLedgerRejected.
var (
	// ErrLedgerInsufficientFunds: the customer's available balance is too low.
	ErrLedgerInsufficientFunds = errors.New("ledger: insufficient funds")
	// ErrLedgerRejected: the ledger refused the request for a reason that
	// repeating it cannot fix (inactive account, unauthorised, malformed).
	ErrLedgerRejected = errors.New("ledger: request rejected")
	// ErrLedgerHoldNotFound: no hold exists under the reference.
	ErrLedgerHoldNotFound = errors.New("ledger: hold not found")
	// ErrLedgerHoldCaptured: the hold was already captured.
	ErrLedgerHoldCaptured = errors.New("ledger: hold already captured")
	// ErrLedgerHoldNotActive: the hold was released or expired.
	ErrLedgerHoldNotActive = errors.New("ledger: hold not active")
	// ErrLedgerNotFound: the journal or account does not exist.
	ErrLedgerNotFound = errors.New("ledger: not found")
)

// JournalRef is a ledger posting reference.
type JournalRef struct {
	OpType string
	OpID   uuid.UUID
	OpStep string
}

// PostRequest asks the ledger to post one journal.
type PostRequest struct {
	Ref          JournalRef
	JournalType  string
	Lines        []domain.JournalLine
	BusinessDate *time.Time
	Narrative    string
}

// PostedJournal identifies a posted journal.
type PostedJournal struct {
	ID       int64
	PostedAt time.Time
	// AlreadyPosted is true when the reference had been posted before; it is
	// counted as a prevented duplicate.
	AlreadyPosted bool
}

// LedgerJournal is a journal read back for reconciliation.
type LedgerJournal struct {
	ID    int64
	Type  string
	Lines []domain.JournalLine
}

// Ledger is what lending needs from the ledger service. Every mutating call
// is idempotent on its reference, so implementations may retry transient
// failures and callers may repeat a call after a crash.
type Ledger interface {
	Post(ctx context.Context, req PostRequest) (PostedJournal, error)
	PlaceHold(ctx context.Context, opType string, opID uuid.UUID, accountCode string, amountMinor int64) error
	CaptureHold(ctx context.Context, holdOpType string, holdOpID uuid.UUID, req PostRequest) (PostedJournal, error)
	ReleaseHold(ctx context.Context, opType string, opID uuid.UUID) error
	// Available returns the account's available balance. It is used only to
	// size a collection attempt; the ledger still decides whether the debit
	// is allowed.
	Available(ctx context.Context, accountCode string) (int64, error)
	// Posted returns an account's posted balance, for reconciliation.
	Posted(ctx context.Context, accountCode string) (int64, error)
	Journal(ctx context.Context, ref JournalRef) (LedgerJournal, error)
}

// CreditBureau fetches a credit report. requestRef is our reference for the
// enquiry: repeating a call with the same reference must return the same
// report and must not count as a second enquiry.
//
// Any error means "no report": the caller never treats a failed or malformed
// response as a clean file.
type CreditBureau interface {
	Name() string
	FetchReport(ctx context.Context, requestRef string, subject BureauSubject) (domain.BureauReport, error)
}

// FraudInput is what the fraud screen looks at. It is limited to the
// customer's own application behaviour; no device content, contacts or
// location are collected.
type FraudInput struct {
	CustomerID       uuid.UUID
	ApplicationID    uuid.UUID
	RequestedMinor   int64
	Applications24h  int
	HasPriorWriteOff bool
}

// FraudResult is the screen's verdict with the signals that produced it.
type FraudResult struct {
	Decision domain.FraudDecision
	Signals  []string
}

// FraudScreener screens an application for fraud risk.
type FraudScreener interface {
	Screen(ctx context.Context, in FraudInput) (FraudResult, error)
}

// ResolvedAccount is the result of a name enquiry.
type ResolvedAccount struct {
	AccountName string
	EnquiryRef  string
}

// PayoutResult is what a provider says about one transfer.
type PayoutResult struct {
	Outcome     domain.ProviderOutcome
	ProviderRef string
	Code        string
}

// SettlementItem is one line of the provider's settlement report.
type SettlementItem struct {
	Reference   string
	ProviderRef string
	AmountMinor int64
	Outcome     domain.ProviderOutcome
	Settled     bool
}

// PayoutInstruction is one transfer to send.
type PayoutInstruction struct {
	// Reference is our reference; it makes Send idempotent at the provider.
	Reference     string
	AmountMinor   int64
	BankCode      string
	AccountNumber string
	// AccountName is the name returned by name enquiry for the account.
	AccountName string
	Narration   string
}

// ErrAccountNotResolved means the provider definitively could not find the
// destination account.
var ErrAccountNotResolved = errors.New("payout: account could not be resolved")

// PayoutProvider sends money to accounts at other banks.
//
// Contract every implementation must honour:
//   - Send is idempotent on reference: a second Send with the same reference
//     never pays twice and reports the first transfer's status.
//   - An error from Send or Query means the outcome is UNKNOWN. It never
//     means the transfer failed. Only an explicit FAILED outcome does.
//   - Query returns NOT_FOUND when the provider has no record of the
//     reference; that alone is not proof that the transfer will not appear.
type PayoutProvider interface {
	Name() string
	ResolveAccount(ctx context.Context, bankCode, accountNumber string) (ResolvedAccount, error)
	Send(ctx context.Context, in PayoutInstruction) (PayoutResult, error)
	Query(ctx context.Context, reference string) (PayoutResult, error)
	SettlementReport(ctx context.Context, businessDate time.Time) ([]SettlementItem, error)
}

// PaymentStatus is what a provider says about one inbound payment.
type PaymentStatus struct {
	Outcome     domain.ProviderOutcome
	AmountMinor int64
	Currency    string
	ProviderRef string
	Code        string
}

// PaymentVerifier answers "did this payment really happen?" for payments a
// customer makes to the bank through an external provider.
//
// Contract every implementation must honour:
//   - VerifyPayment asks the provider for its own record of the reference.
//     It never relies on anything the customer or a webhook claimed.
//   - An error means the answer is UNKNOWN. It never means the payment
//     failed. NOT_FOUND means the provider has no record of the reference
//     yet, which is normal before the customer has paid.
//   - A SUCCESS answer carries the amount and currency the provider
//     actually collected.
type PaymentVerifier interface {
	Name() string
	VerifyPayment(ctx context.Context, reference string) (PaymentStatus, error)
}

// ErrCallbackRejected means a provider callback or webhook failed
// authentication (bad signature, stale timestamp). Nothing is done with it.
var ErrCallbackRejected = errors.New("provider callback rejected")

// Metrics receives lending's operational signals. Label values passed here
// are bounded sets (states, outcomes, kinds); identifiers are never labels.
type Metrics interface {
	ApplicationDecided(outcome string, decisionTime time.Duration)
	StageDuration(stage string, d time.Duration, ok bool)
	LoanDisbursed(acceptToPosted, systemTime time.Duration)
	IntentCompleted(kind string, outcome string)
	IdempotentReplay(endpoint string)
	DuplicatePostingPrevented(kind string)
	ProviderCall(provider, operation, outcome string, d time.Duration)
	PayoutStateChanged(to string)
	CallbackReceived(outcome string)
	ExternalPaymentStateChanged(to string)
	ReconException(kind string)
	EventConsumed(eventType, outcome string, age time.Duration)
}
