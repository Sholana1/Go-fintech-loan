package ledger_test

import (
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "bankplatform.internal/gen/bank/common/v1"
	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/services/ledger/contract"
)

const (
	debit  = ledgerv1.Direction_DIRECTION_DEBIT
	credit = ledgerv1.Direction_DIRECTION_CREDIT
)

func ngn(minor int64) *commonv1.Money { return &commonv1.Money{MinorUnits: minor, Currency: "NGN"} }

func line(code string, d ledgerv1.Direction, minor int64) *ledgerv1.Line {
	return &ledgerv1.Line{AccountCode: code, Direction: d, Amount: ngn(minor)}
}

// repayment builds a LOAN_REPAYMENT journal: the simplest journal the lending
// service may post that debits a customer account.
func repayment(ref *ledgerv1.PostingRef, customer string, minor int64) *ledgerv1.PostJournalRequest {
	return &ledgerv1.PostJournalRequest{
		Ref: ref, JournalType: contract.JournalLoanRepayment,
		Lines: []*ledgerv1.Line{line(customer, debit, minor), line(contract.AccountLoansPrincipal, credit, minor)},
	}
}

func newRef(opType string) *ledgerv1.PostingRef {
	return &ledgerv1.PostingRef{OpType: opType, OpId: uuid.NewString(), OpStep: "POST"}
}

func wantReason(t *testing.T, err error, code codes.Code, reason string) {
	t.Helper()
	if status.Code(err) != code || contract.ReasonOf(err) != reason {
		t.Fatalf("want %s/%s, got %s/%q (%v)", code, reason, status.Code(err), contract.ReasonOf(err), err)
	}
}

func expectOKOrFunds(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return true
	}
	if contract.ReasonOf(err) != contract.ReasonInsufficientFunds {
		t.Errorf("unexpected error: %v", err)
	}
	return false
}
