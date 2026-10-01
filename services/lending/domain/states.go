package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidTransition is returned when a state change is not in the
// machine. Persistence additionally enforces transitions with compare-and-set
// updates, so two workers cannot both move the same row.
var ErrInvalidTransition = errors.New("invalid state transition")

// ApplicationState is the lifecycle of a loan application.
//
//	SUBMITTED -> ASSESSING -> DECLINED
//	                       -> REFERRED -> OFFERED | DECLINED
//	                       -> OFFERED  -> ACCEPTED -> DISBURSED | DISBURSEMENT_FAILED
//	                                   -> EXPIRED
//	SUBMITTED | ASSESSING | REFERRED -> EXPIRED   (dependency unavailable too long)
//	SUBMITTED | ASSESSING | REFERRED | OFFERED -> CANCELLED (by the customer)
type ApplicationState string

const (
	AppSubmitted          ApplicationState = "SUBMITTED"
	AppAssessing          ApplicationState = "ASSESSING"
	AppDeclined           ApplicationState = "DECLINED"
	AppReferred           ApplicationState = "REFERRED"
	AppOffered            ApplicationState = "OFFERED"
	AppAccepted           ApplicationState = "ACCEPTED"
	AppDisbursed          ApplicationState = "DISBURSED"
	AppExpired            ApplicationState = "EXPIRED"
	AppCancelled          ApplicationState = "CANCELLED"
	AppDisbursementFailed ApplicationState = "DISBURSEMENT_FAILED"
)

var applicationTransitions = map[ApplicationState][]ApplicationState{
	AppSubmitted: {AppAssessing, AppExpired, AppCancelled},
	AppAssessing: {AppAssessing, AppDeclined, AppReferred, AppOffered, AppExpired, AppCancelled},
	AppReferred:  {AppOffered, AppDeclined, AppExpired, AppCancelled},
	AppOffered:   {AppAccepted, AppExpired, AppCancelled},
	AppAccepted:  {AppDisbursed, AppDisbursementFailed},
}

// CanTransition reports whether an application may move from -> to.
func (from ApplicationState) CanTransition(to ApplicationState) bool {
	for _, s := range applicationTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Open reports whether the application still occupies the customer's single
// application slot for the product.
func (s ApplicationState) Open() bool {
	switch s {
	case AppSubmitted, AppAssessing, AppReferred, AppOffered, AppAccepted:
		return true
	}
	return false
}

// Application is a customer's request for a loan.
type Application struct {
	ID                       uuid.UUID
	CustomerID               uuid.UUID
	ProductID                string
	ProductVersion           int
	RequestedPrincipalMinor  int64
	TenorMonths              int
	StatedMonthlyIncomeMinor int64
	State                    ApplicationState
	// StateReason carries the reason codes of a terminal or waiting state.
	StateReason []string
	// WaitingOn names the dependency an ASSESSING application is waiting for.
	WaitingOn string
	// ConsentRef identifies the customer's recorded consent to the credit
	// bureau enquiry and to automated assessment.
	ConsentRef  uuid.UUID
	SubmittedAt time.Time  // T0
	DecidedAt   *time.Time // T1
	ExpiresAt   time.Time
	Attempts    int
	Version     int64
}

// OfferState is the lifecycle of an offer.
type OfferState string

const (
	OfferOpen     OfferState = "OFFERED"
	OfferAccepted OfferState = "ACCEPTED"
	OfferExpired  OfferState = "EXPIRED"
	OfferVoided   OfferState = "VOIDED" // superseded or application cancelled
)

// Offer is an immutable set of terms shown to the customer. Re-pricing never
// edits an offer; it voids it and creates a new one.
type Offer struct {
	ID                     uuid.UUID
	ApplicationID          uuid.UUID
	DecisionID             uuid.UUID
	CustomerID             uuid.UUID
	PrincipalMinor         int64
	TenorMonths            int
	MonthlyRateBps         int
	OriginationFeeMinor    int64
	NetDisbursementMinor   int64
	InstalmentMinor        int64
	TotalInterestMinor     int64
	TotalRepayableMinor    int64
	NominalAnnualRateBps   int
	EffectiveAnnualCostBps int
	Disclosure             []byte // canonical JSON shown to the customer
	DisclosureHash         string // hex SHA-256 of Disclosure
	State                  OfferState
	ExpiresAt              time.Time
	AcceptedAt             *time.Time // T2
	CreatedAt              time.Time
}

// LoanState is the lifecycle of a booked loan.
//
//	PENDING_DISBURSEMENT -> ACTIVE <-> IN_ARREARS -> WRITTEN_OFF -> CLOSED
//	                     -> DISBURSEMENT_FAILED      ACTIVE | IN_ARREARS -> CLOSED
type LoanState string

const (
	LoanPendingDisbursement LoanState = "PENDING_DISBURSEMENT"
	LoanActive              LoanState = "ACTIVE"
	LoanInArrears           LoanState = "IN_ARREARS"
	LoanClosed              LoanState = "CLOSED"
	LoanWrittenOff          LoanState = "WRITTEN_OFF"
	LoanDisbursementFailed  LoanState = "DISBURSEMENT_FAILED"
)

var loanTransitions = map[LoanState][]LoanState{
	LoanPendingDisbursement: {LoanActive, LoanDisbursementFailed},
	LoanActive:              {LoanInArrears, LoanClosed},
	LoanInArrears:           {LoanActive, LoanClosed, LoanWrittenOff},
	LoanWrittenOff:          {LoanClosed},
}

func (from LoanState) CanTransition(to LoanState) bool {
	for _, s := range loanTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Servicing reports whether the loan is being repaid normally (interest
// accrues, instalments fall due, arrears are classified).
func (s LoanState) Servicing() bool { return s == LoanActive || s == LoanInArrears }

// Loan is a booked loan.
type Loan struct {
	ID                  uuid.UUID
	ApplicationID       uuid.UUID
	OfferID             uuid.UUID
	CustomerID          uuid.UUID
	ProductID           string
	ProductVersion      int
	PrincipalMinor      int64
	MonthlyRateBps      int
	TenorMonths         int
	OriginationFeeMinor int64
	State               LoanState
	ScheduleVersion     int
	AutoDebitAuthorised bool
	DepositAccountCode  string
	AcceptedOn          time.Time  // business date the schedule is anchored on
	DisbursedAt         *time.Time // T3
	DaysPastDue         int
	ArrearsBucket       string
	Restructured        bool
	// Written-off amounts and what has been recovered since.
	WrittenOffMinor int64
	RecoveredMinor  int64
	ClosedAt        *time.Time
	// AccrualCatchup is true when the loan left servicing with earned
	// interest that the accrual job has not yet recognised.
	AccrualCatchup bool
	Version        int64
}

// PayoutState is the lifecycle of an external payout of loan proceeds.
//
//	PENDING (loan not booked yet) -> READY -> HELD -> SENDING -> SUCCEEDED -> SETTLED
//	                                       -> FAILED          -> FAILED
//	                                                          -> UNKNOWN -> SUCCEEDED | FAILED
//
// UNKNOWN is a real state, not an error: the provider may or may not have
// paid. Funds stay held until evidence resolves it one way or the other.
type PayoutState string

const (
	PayoutPending   PayoutState = "PENDING"
	PayoutReady     PayoutState = "READY"
	PayoutHeld      PayoutState = "HELD"
	PayoutSending   PayoutState = "SENDING"
	PayoutSucceeded PayoutState = "SUCCEEDED"
	PayoutFailed    PayoutState = "FAILED"
	PayoutUnknown   PayoutState = "UNKNOWN"
	PayoutSettled   PayoutState = "SETTLED"
	PayoutCancelled PayoutState = "CANCELLED" // the loan was never booked
)

var payoutTransitions = map[PayoutState][]PayoutState{
	PayoutPending:   {PayoutReady, PayoutCancelled},
	PayoutReady:     {PayoutHeld, PayoutFailed},
	PayoutHeld:      {PayoutSending},
	PayoutSending:   {PayoutSucceeded, PayoutFailed, PayoutUnknown},
	PayoutUnknown:   {PayoutSucceeded, PayoutFailed},
	PayoutSucceeded: {PayoutSettled},
	// FAILED -> SUCCEEDED exists for exactly one case: the provider proves,
	// after we recorded a failure and released the hold, that it did pay.
	// The move is only made together with a late-capture journal that debits
	// the customer; see app.resolveLateSuccess. There is deliberately no
	// SUCCEEDED -> FAILED: a completed external payment cannot be undone by
	// changing our records.
	PayoutFailed: {PayoutSucceeded},
}

func (from PayoutState) CanTransition(to PayoutState) bool {
	for _, s := range payoutTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Terminal reports whether no further automatic progress is expected.
func (s PayoutState) Terminal() bool {
	return s == PayoutSettled || s == PayoutFailed || s == PayoutCancelled
}

// ProviderOutcome is what a provider says about a payout, whether in the
// send response, a status query, a callback, or a settlement report.
type ProviderOutcome string

const (
	ProviderSuccess  ProviderOutcome = "SUCCESS"
	ProviderFailed   ProviderOutcome = "FAILED"
	ProviderPending  ProviderOutcome = "PENDING"
	ProviderNotFound ProviderOutcome = "NOT_FOUND"
)

// Payout is an instruction to send loan proceeds to the customer's own
// account at another bank.
type Payout struct {
	ID            uuid.UUID
	LoanID        uuid.UUID
	CustomerID    uuid.UUID
	AmountMinor   int64
	BankCode      string
	AccountNumber string
	AccountName   string // as returned by name enquiry
	// Reference is our reference for the transfer. It is generated and
	// stored before the provider is called and never changes: every send,
	// query, callback and report line is matched on it.
	Reference    string
	ProviderRef  string
	State        PayoutState
	LastCode     string
	Attempts     int
	StateChanged time.Time
	CreatedAt    time.Time
}

// ValidateTransition returns ErrInvalidTransition with context.
func ValidateTransition[S interface {
	~string
	CanTransition(S) bool
}](from, to S) error {
	if !from.CanTransition(to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}
