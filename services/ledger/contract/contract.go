// Package contract is the part of the ledger that its clients compile
// against, in addition to the generated protobuf code: stable error reasons,
// event names and payloads, and account-code conventions.
//
// It depends on nothing inside the ledger service, so importing it does not
// pull ledger internals into another service's binary. Everything here is a
// compatibility commitment: names are never renamed or reused.
package contract

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
)

// ErrorDomain is the ErrorInfo domain of ledger errors.
const ErrorDomain = "ledger.bankplatform.internal"

// Stable machine-readable error reasons carried in google.rpc.ErrorInfo.
const (
	ReasonInvalid           = "INVALID_REQUEST"
	ReasonUnbalanced        = "JOURNAL_UNBALANCED"
	ReasonNotAuthorised     = "NOT_AUTHORISED"
	ReasonAccountNotFound   = "ACCOUNT_NOT_FOUND"
	ReasonAccountNotActive  = "ACCOUNT_NOT_ACTIVE"
	ReasonCurrencyMismatch  = "CURRENCY_MISMATCH"
	ReasonInsufficientFunds = "INSUFFICIENT_FUNDS"
	ReasonRefReused         = "POSTING_REF_REUSED"
	ReasonHoldNotFound      = "HOLD_NOT_FOUND"
	ReasonHoldNotActive     = "HOLD_NOT_ACTIVE"
	ReasonHoldCaptured      = "HOLD_ALREADY_CAPTURED"
	ReasonJournalNotFound   = "JOURNAL_NOT_FOUND"
	ReasonAccountConflict   = "ACCOUNT_CONFLICT"
	ReasonInternal          = "INTERNAL"
)

// ReasonOf extracts the stable reason from an error returned by the ledger,
// or "" when the error carries none (for example a transport failure).
// Clients branch on this, never on message text.
func ReasonOf(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == ErrorDomain {
			return info.GetReason()
		}
	}
	return ""
}

// Kafka topic and event type for posted journals.
const (
	TopicJournals      = "ledger.journal.v1"
	EventJournalPosted = "ledger.journal.posted"
)

// JournalPosted is the payload of EventJournalPosted, schema version 1.
// It carries account codes and amounts only: no names or other personal data.
// Compatibility: fields are only added; consumers must ignore unknown fields.
type JournalPosted struct {
	JournalID    int64               `json:"journal_id"`
	JournalType  string              `json:"journal_type"`
	BusinessDate string              `json:"business_date"`
	OpType       string              `json:"op_type"`
	OpID         string              `json:"op_id"`
	OpStep       string              `json:"op_step"`
	Lines        []JournalPostedLine `json:"lines"`
}

type JournalPostedLine struct {
	AccountCode string `json:"account_code"`
	Direction   string `json:"direction"` // "DEBIT" or "CREDIT"
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// CustomerDepositCode returns the ledger code of a customer's main deposit
// account. The code is derived, never supplied by an end user, so a customer
// cannot name someone else's account.
func CustomerDepositCode(customerID string) string {
	return "CUST:" + customerID + ":MAIN"
}

// System accounts used by the personal-loan product.
const (
	AccountCashSettlementBank      = "SYS:CASH_SETTLEMENT_BANK"
	AccountLoansPrincipal          = "SYS:LOANS_PRINCIPAL"
	AccountLoansInterestReceivable = "SYS:LOANS_INTEREST_RECEIVABLE"
	AccountLoansFeesReceivable     = "SYS:LOANS_FEES_RECEIVABLE"
	AccountPayoutClearing          = "SYS:PAYOUT_CLEARING"
	AccountInterestIncome          = "SYS:INTEREST_INCOME"
	AccountFeeIncome               = "SYS:FEE_INCOME"
	AccountRecoveriesIncome        = "SYS:RECOVERIES_INCOME"
	AccountLoanWriteOffExpense     = "SYS:LOAN_WRITE_OFF_EXPENSE"
	// AccountCollectionsClearing is what a payment provider owes the bank
	// for customer payments it has confirmed but not yet settled.
	AccountCollectionsClearing = "SYS:COLLECTIONS_CLEARING"
)

// Journal types the lending service is permitted to post.
const (
	JournalLoanDisbursement  = "LOAN_DISBURSEMENT"
	JournalLoanRepayment     = "LOAN_REPAYMENT"
	JournalLoanAccrual       = "LOAN_INTEREST_ACCRUAL"
	JournalLoanFee           = "LOAN_FEE_ASSESSMENT"
	JournalLoanWriteOff      = "LOAN_WRITE_OFF"
	JournalLoanRecovery      = "LOAN_RECOVERY"
	JournalPayoutCapture     = "LOAN_PAYOUT_CAPTURE"
	JournalPayoutLateCapture = "LOAN_PAYOUT_LATE_CAPTURE"
	JournalPayoutSettlement  = "LOAN_PAYOUT_SETTLEMENT"
	HoldLoanPayout           = "LOAN_PAYOUT"
	// JournalExternalCollection credits a customer with a payment an
	// external provider has confirmed.
	JournalExternalCollection = "EXTERNAL_COLLECTION"
)

// Transfers between customers and to other banks (the payments service).
const (
	// AccountTransferClearing is what the bank owes the payment rail for
	// outbound transfers the rail has confirmed.
	AccountTransferClearing = "SYS:TRANSFER_CLEARING"

	// JournalP2PTransfer moves money from one customer to another.
	JournalP2PTransfer            = "P2P_TRANSFER"
	JournalP2POutboundCapture     = "P2P_OUTBOUND_CAPTURE"
	JournalP2POutboundLateCapture = "P2P_OUTBOUND_LATE_CAPTURE"
	HoldP2POutbound               = "P2P_OUTBOUND"
)
