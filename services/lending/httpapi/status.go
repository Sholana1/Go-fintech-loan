package httpapi

import (
	"bankplatform.internal/services/lending/domain"
)

// Customer-visible statuses are a deliberate reduction of internal states.
// In particular an unknown external outcome is shown as PROCESSING, never as
// FAILED, and a loan is shown as DISBURSED only once the ledger has posted.

func applicationStatus(s domain.ApplicationState) string {
	switch s {
	case domain.AppSubmitted, domain.AppAssessing:
		return "PROCESSING"
	case domain.AppReferred:
		return "UNDER_REVIEW"
	case domain.AppOffered:
		return "OFFER_READY"
	case domain.AppAccepted:
		return "DISBURSING"
	case domain.AppDisbursed:
		return "DISBURSED"
	case domain.AppDeclined:
		return "DECLINED"
	case domain.AppExpired:
		return "EXPIRED"
	case domain.AppCancelled:
		return "CANCELLED"
	default:
		return "FAILED"
	}
}

func loanStatus(s domain.LoanState) string {
	switch s {
	case domain.LoanPendingDisbursement:
		return "DISBURSING"
	case domain.LoanActive:
		return "ACTIVE"
	case domain.LoanInArrears:
		return "OVERDUE"
	case domain.LoanClosed:
		return "CLOSED"
	case domain.LoanWrittenOff:
		return "IN_RECOVERY"
	default:
		return "DISBURSEMENT_FAILED"
	}
}

func payoutStatus(s domain.PayoutState) string {
	switch s {
	case domain.PayoutSucceeded, domain.PayoutSettled:
		return "SENT"
	case domain.PayoutFailed:
		return "FAILED"
	case domain.PayoutCancelled:
		return "CANCELLED"
	default: // PENDING, READY, HELD, SENDING, UNKNOWN
		return "PROCESSING"
	}
}
