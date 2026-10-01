package ledger_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/money"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/ledger/domain"
	"bankplatform.internal/services/ledger/ledgertest"
	"bankplatform.internal/services/ledger/postgres"
)

// Two or more concurrent debits can never spend the same available funds.
func TestConcurrentDebitsNeverOverspend(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 50_000)

	const workers = 40
	var ok, insufficient, other atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := lending.PostJournal(context.Background(), repayment(newRef("TEST_DEBIT"), a, 10_000))
			switch {
			case err == nil:
				ok.Add(1)
			case contract.ReasonOf(err) == contract.ReasonInsufficientFunds:
				insufficient.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != 5 || insufficient.Load() != workers-5 || other.Load() != 0 {
		t.Fatalf("ok=%d insufficient=%d other=%d; want 5/%d/0", ok.Load(), insufficient.Load(), other.Load(), workers-5)
	}
	if posted, held := env.Balance(t, a); posted != 0 || held != 0 {
		t.Fatalf("final balance posted=%d held=%d, want 0/0", posted, held)
	}
	env.AssertInvariants(t)
}

// Transfers in opposite directions between the same two accounts must queue,
// not deadlock. This exercises the deterministic lock order directly at the
// store, because no journal type touching two customers exists yet.
func TestOpposingPostingsDoNotDeadlock(t *testing.T) {
	env := ledgertest.Start(t)
	_, a := env.OpenCustomer(t)
	_, b := env.OpenCustomer(t)
	env.Fund(t, a, 1_000_000)
	env.Fund(t, b, 1_000_000)

	var retries atomic.Int64
	store := postgres.NewStore(env.Pool, time.Now, func(string, int) { retries.Add(1) })

	post := func(from, to string) error {
		amt := money.MustNew(100, money.NGN)
		_, err := store.PostJournal(context.Background(), postgres.PostCommand{
			Ref:         domain.PostingRef{OpType: "TEST_TRANSFER", OpID: uuid.New(), OpStep: "POST"},
			JournalType: "TEST_TRANSFER", Caller: "test",
			Lines: []domain.Line{{AccountCode: from, Direction: domain.Debit, Amount: amt}, {AccountCode: to, Direction: domain.Credit, Amount: amt}},
		})
		return err
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := post(a, b); err != nil {
				t.Errorf("a->b: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := post(b, a); err != nil {
				t.Errorf("b->a: %v", err)
			}
		}()
	}
	wg.Wait()

	if retries.Load() != 0 {
		t.Errorf("%d deadlock/serialization retries occurred; lock ordering should make this 0", retries.Load())
	}
	pa, _ := env.Balance(t, a)
	pb, _ := env.Balance(t, b)
	if pa != 1_000_000 || pb != 1_000_000 {
		t.Fatalf("balances a=%d b=%d, want both 1000000", pa, pb)
	}
	env.AssertInvariants(t)
}

// A capture and a release racing for the same hold: exactly one wins and the
// money is either debited once or not at all.
func TestCaptureAndReleaseRace(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 100_000*20)

	for i := range 20 {
		holdRef := &ledgerv1.HoldRef{OpType: contract.HoldLoanPayout, OpId: uuid.NewString()}
		if _, err := lending.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{Ref: holdRef, AccountCode: a, Amount: ngn(100_000)}); err != nil {
			t.Fatal(err)
		}
		before, _ := env.Balance(t, a)

		var capErr, relErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, capErr = lending.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{
				Hold: holdRef, Ref: newRef("TEST_CAPTURE"), JournalType: contract.JournalPayoutCapture,
				Lines: []*ledgerv1.Line{line(a, debit, 100_000), line(contract.AccountPayoutClearing, credit, 100_000)},
			})
		}()
		go func() {
			defer wg.Done()
			_, relErr = lending.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{Hold: holdRef})
		}()
		wg.Wait()

		after, held := env.Balance(t, a)
		switch {
		case capErr == nil && contract.ReasonOf(relErr) == contract.ReasonHoldCaptured:
			if after != before-100_000 {
				t.Fatalf("round %d: capture won but balance moved %d", i, before-after)
			}
		case relErr == nil && contract.ReasonOf(capErr) == contract.ReasonHoldNotActive:
			if after != before {
				t.Fatalf("round %d: release won but balance moved %d", i, before-after)
			}
		default:
			t.Fatalf("round %d: unexpected outcome capture=%v release=%v", i, capErr, relErr)
		}
		if held != 0 {
			t.Fatalf("round %d: held=%d after hold closed", i, held)
		}
	}
	env.AssertInvariants(t)
}
