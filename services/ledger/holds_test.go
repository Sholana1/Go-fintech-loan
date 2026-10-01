package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/ledger/ledgertest"
)

func TestHoldLifecycle(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 40_000)

	holdRef := &ledgerv1.HoldRef{OpType: contract.HoldLoanPayout, OpId: uuid.NewString()}
	placed, err := lending.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{Ref: holdRef, AccountCode: a, Amount: ngn(35_000)})
	if err != nil || placed.GetAlreadyPlaced() {
		t.Fatalf("place hold: %v %+v", err, placed)
	}

	// Held funds cannot be spent by a posting or by another hold.
	_, err = lending.PostJournal(ctx, repayment(newRef("TEST_DEBIT"), a, 10_000))
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonInsufficientFunds)
	_, err = lending.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{
		Ref: &ledgerv1.HoldRef{OpType: contract.HoldLoanPayout, OpId: uuid.NewString()}, AccountCode: a, Amount: ngn(10_000)})
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonInsufficientFunds)

	// Repeating the placement returns the same hold and reserves nothing more.
	again, err := lending.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{Ref: holdRef, AccountCode: a, Amount: ngn(35_000)})
	if err != nil || !again.GetAlreadyPlaced() || again.GetHoldId() != placed.GetHoldId() {
		t.Fatalf("repeat placement: %v %+v", err, again)
	}
	// The same reference with a different amount is a conflict.
	_, err = lending.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{Ref: holdRef, AccountCode: a, Amount: ngn(1)})
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonRefReused)
	if _, held := env.Balance(t, a); held != 35_000 {
		t.Fatalf("held %d, want 35000", held)
	}

	capture := func(ref *ledgerv1.PostingRef, minor int64) (*ledgerv1.CaptureHoldResponse, error) {
		return lending.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{
			Hold: holdRef, Ref: ref, JournalType: contract.JournalPayoutCapture,
			Lines: []*ledgerv1.Line{line(a, debit, minor), line(contract.AccountPayoutClearing, credit, minor)},
		})
	}

	// Capturing more than was held is refused.
	_, err = capture(newRef("TEST_CAPTURE"), 35_001)
	wantReason(t, err, codes.InvalidArgument, contract.ReasonInvalid)

	// Partial capture: 30,000 is debited and the remaining 5,000 is released.
	capRef := newRef("TEST_CAPTURE")
	first, err := capture(capRef, 30_000)
	if err != nil || first.GetAlreadyCaptured() {
		t.Fatalf("capture: %v %+v", err, first)
	}
	if posted, held := env.Balance(t, a); posted != 10_000 || held != 0 {
		t.Fatalf("after capture posted=%d held=%d, want 10000/0", posted, held)
	}

	// Repeating the capture is a no-op that returns the same journal.
	second, err := capture(capRef, 30_000)
	if err != nil || !second.GetAlreadyCaptured() || second.GetJournalId() != first.GetJournalId() {
		t.Fatalf("repeat capture: %v %+v", err, second)
	}
	// A different capture of the same hold is refused.
	_, err = capture(newRef("TEST_CAPTURE"), 1_000)
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonHoldCaptured)
	// Releasing a captured hold must fail loudly: the money was spent.
	_, err = lending.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{Hold: holdRef})
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonHoldCaptured)

	// Release path.
	relRef := &ledgerv1.HoldRef{OpType: contract.HoldLoanPayout, OpId: uuid.NewString()}
	if _, err := lending.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{Ref: relRef, AccountCode: a, Amount: ngn(4_000)}); err != nil {
		t.Fatal(err)
	}
	rel, err := lending.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{Hold: relRef})
	if err != nil || rel.GetAlreadyClosed() || rel.GetStatus() != ledgerv1.HoldStatus_HOLD_STATUS_RELEASED {
		t.Fatalf("release: %v %+v", err, rel)
	}
	rel, err = lending.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{Hold: relRef})
	if err != nil || !rel.GetAlreadyClosed() {
		t.Fatalf("repeat release: %v %+v", err, rel)
	}
	// Capturing a released hold is refused.
	_, err = lending.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{
		Hold: relRef, Ref: newRef("TEST_CAPTURE"), JournalType: contract.JournalPayoutCapture,
		Lines: []*ledgerv1.Line{line(a, debit, 4_000), line(contract.AccountPayoutClearing, credit, 4_000)},
	})
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonHoldNotActive)

	_, err = lending.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{Hold: &ledgerv1.HoldRef{OpType: contract.HoldLoanPayout, OpId: uuid.NewString()}})
	wantReason(t, err, codes.NotFound, contract.ReasonHoldNotFound)

	if posted, held := env.Balance(t, a); posted != 10_000 || held != 0 {
		t.Fatalf("final posted=%d held=%d, want 10000/0", posted, held)
	}
	env.AssertInvariants(t)
}
