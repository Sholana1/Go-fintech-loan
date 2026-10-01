package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Errors returned by lending use cases. The HTTP layer maps each to one
// stable API error code.
var (
	ErrValidation          = errors.New("validation failed")
	ErrNotFound            = errors.New("not found")
	ErrApplicationOpen     = errors.New("an application is already open for this product")
	ErrIdempotencyMismatch = errors.New("idempotency key was used with a different request")
	ErrOfferNotOpen        = errors.New("offer is no longer open")
	ErrOfferExpired        = errors.New("offer has expired")
	ErrDisclosureMismatch  = errors.New("disclosure hash does not match the offer")
	ErrStepUpFailed        = errors.New("step-up authentication failed")
	ErrStepUpLocked        = errors.New("credential is temporarily locked")
	ErrDestinationRejected = errors.New("destination account could not be verified as the customer's own")
	ErrOperationInProgress = errors.New("another operation on this loan is in progress")
	ErrLoanNotRepayable    = errors.New("loan is not in a repayable state")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrForbidden           = errors.New("not permitted")
	ErrConflict            = errors.New("conflicting state")
	// ErrDependencyUnavailable means a required dependency could not answer.
	// The request was not applied and may be retried with the same
	// idempotency key.
	ErrDependencyUnavailable = errors.New("a required dependency is unavailable")
)

// BureauReport is the normalised content of a credit-bureau response. The
// raw response is stored alongside it, unmodified.
type BureauReport struct {
	ID          uuid.UUID
	Provider    string
	RequestRef  string
	ProviderRef string
	FetchedAt   time.Time
	// Score is nil when the bureau holds too little history to score.
	Score                   *int
	WorstDelinquencyDays12m int
	MonthlyObligationsMinor int64
	TotalOutstandingMinor   int64
	ActiveLoans             int
	Raw                     []byte
}

// DecisionSnapshot is the immutable record of one credit decision: exactly
// what the policy saw and what it concluded.
type DecisionSnapshot struct {
	ID             uuid.UUID
	ApplicationID  uuid.UUID
	Decision       Decision
	PolicyVersion  string
	ProductID      string
	ProductVersion int
	Features       Features
	InputRefs      map[string]string
	DecidedBy      string // "AUTO" or a staff id
	ReviewerNote   string
	CreatedAt      time.Time
}

// IntentKind names what a posting intent does when it has been posted.
type IntentKind string

const (
	IntentDisbursement IntentKind = "DISBURSEMENT"
	IntentRepayment    IntentKind = "REPAYMENT"
	IntentLateFee      IntentKind = "LATE_FEE"
	IntentAccrual      IntentKind = "ACCRUAL"
	IntentWriteOff     IntentKind = "WRITE_OFF"
	IntentRecovery     IntentKind = "RECOVERY"
)

type IntentState string

const (
	IntentPending  IntentState = "PENDING"
	IntentPosted   IntentState = "POSTED"
	IntentRejected IntentState = "REJECTED"
)

// JournalLine is one line of the journal an intent will post.
type JournalLine struct {
	AccountCode string `json:"account_code"`
	Direction   string `json:"direction"` // "DEBIT" | "CREDIT"
	AmountMinor int64  `json:"amount_minor"`
}

// PostingIntent is a durable instruction to post one journal to the ledger.
// Its ID is the ledger posting reference, so posting it any number of times
// produces one journal.
type PostingIntent struct {
	ID           uuid.UUID
	Kind         IntentKind
	LoanID       *uuid.UUID
	JournalType  string
	Lines        []JournalLine
	BusinessDate *time.Time
	Payload      json.RawMessage
	State        IntentState
	RejectReason string
	JournalID    *int64
	Attempts     int
	CreatedAt    time.Time
}

type RepaymentState string

const (
	RepaymentPending   RepaymentState = "PENDING"
	RepaymentAllocated RepaymentState = "ALLOCATED"
	RepaymentRejected  RepaymentState = "REJECTED"
)

// Repayment sources. Both are authorised by the customer: CUSTOMER is an
// explicit request; AUTO_DEBIT is the scheduled debit of the customer's own
// account that they authorised when accepting the offer.
const (
	SourceCustomer  = "CUSTOMER"
	SourceAutoDebit = "AUTO_DEBIT"
)

// Repayment is one payment towards a loan.
type Repayment struct {
	ID             uuid.UUID
	LoanID         uuid.UUID
	CustomerID     uuid.UUID
	Source         string
	RequestedMinor int64
	AppliedMinor   int64
	UnappliedMinor int64
	Allocation     Allocation
	IsRecovery     bool // payment on a written-off loan
	State          RepaymentState
	RejectReason   string
	JournalID      *int64
	BusinessDate   time.Time
	IntentID       uuid.UUID
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

type AdminActionKind string

const (
	ActionWriteOff    AdminActionKind = "WRITE_OFF"
	ActionRestructure AdminActionKind = "RESTRUCTURE"
)

type AdminActionState string

const (
	ActionProposed AdminActionState = "PROPOSED"
	ActionApproved AdminActionState = "APPROVED"
	ActionRejected AdminActionState = "REJECTED"
	ActionExecuted AdminActionState = "EXECUTED"
	ActionFailed   AdminActionState = "FAILED"
)

// AdminAction is a staff-initiated change to a loan that needs a second
// person's approval before it takes effect.
type AdminAction struct {
	ID           uuid.UUID
	Kind         AdminActionKind
	LoanID       uuid.UUID
	Params       json.RawMessage
	Reason       string
	State        AdminActionState
	MakerID      uuid.UUID
	CheckerID    *uuid.UUID
	DecisionNote string
	CreatedAt    time.Time
	DecidedAt    *time.Time
	ExecutedAt   *time.Time
}

// Reconciliation exception kinds.
const (
	// The provider reports success for a payout we recorded as failed and
	// whose hold we released.
	ExceptionLateSuccess = "PAYOUT_LATE_SUCCESS"
	// The provider reports failure for a payout we captured as successful.
	ExceptionProviderConflict = "PAYOUT_PROVIDER_CONFLICT"
	// A payout has been unresolved for longer than the alert threshold.
	ExceptionUnresolved = "PAYOUT_UNRESOLVED"
	// The provider's report contains a transfer we have no record of.
	ExceptionUnknownAtUs = "PAYOUT_UNKNOWN_REFERENCE"
	// We recorded success but the provider's report does not contain it.
	ExceptionMissingAtProvider = "PAYOUT_MISSING_AT_PROVIDER"
	// Amount differs between our record and the provider's.
	ExceptionAmountMismatch = "PAYOUT_AMOUNT_MISMATCH"
	// A loan-level posting was rejected by the ledger.
	ExceptionPostingRejected = "POSTING_REJECTED"
	// A lending subledger total differs from the ledger control account.
	ExceptionControlAccount = "CONTROL_ACCOUNT_MISMATCH"
	// A loan marked disbursed or a repayment marked allocated has no journal
	// in the ledger, or its journal differs.
	ExceptionJournalMismatch = "JOURNAL_MISMATCH"
)

// ReconException is a discrepancy that needs a person to look at it.
type ReconException struct {
	ID             uuid.UUID
	Kind           string
	EntityType     string
	EntityID       string
	AmountMinor    int64
	Detail         json.RawMessage
	State          string // OPEN | RESOLVED
	OpenedAt       time.Time
	ResolvedAt     *time.Time
	ResolvedBy     string
	ResolutionNote string
}
