package ledger_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	"bankplatform.internal/platform/money"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/ledger/domain"
	"bankplatform.internal/services/ledger/ledgertest"
	"bankplatform.internal/services/ledger/postgres"
)

// The same operation submitted many times concurrently posts exactly once.
func TestSameReferencePostsExactlyOnce(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 100_000)

	ref := newRef("TEST_DEBIT")
	const workers = 20
	ids := make([]int64, workers)
	var fresh atomic.Int64
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := lending.PostJournal(context.Background(), repayment(ref, a, 10_000))
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			ids[i] = resp.GetJournalId()
			if !resp.GetAlreadyPosted() {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()

	if fresh.Load() != 1 {
		t.Fatalf("%d requests reported a new posting, want exactly 1", fresh.Load())
	}
	for _, id := range ids {
		if id != ids[0] || id == 0 {
			t.Fatalf("requests returned different journals: %v", ids)
		}
	}
	if posted, _ := env.Balance(t, a); posted != 90_000 {
		t.Fatalf("balance %d, want 90000 (debited once)", posted)
	}
	env.AssertInvariants(t)
}

func TestReferenceReusedWithDifferentContentIsRejected(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 100_000)

	ref := newRef("TEST_DEBIT")
	if _, err := lending.PostJournal(ctx, repayment(ref, a, 10_000)); err != nil {
		t.Fatal(err)
	}
	_, err := lending.PostJournal(ctx, repayment(ref, a, 10_001))
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonRefReused)

	if posted, _ := env.Balance(t, a); posted != 90_000 {
		t.Fatalf("balance %d, want 90000", posted)
	}
}

// The store's retry wrapper surfaces non-retryable errors unchanged.
func TestDomainErrorsAreNotRetried(t *testing.T) {
	env := ledgertest.Start(t)
	var retries atomic.Int64
	store := postgres.NewStore(env.Pool, time.Now, func(string, int) { retries.Add(1) })
	_, a := env.OpenCustomer(t)

	amt := money.MustNew(100, money.NGN)
	_, err := store.PostJournal(context.Background(), postgres.PostCommand{
		Ref:         domain.PostingRef{OpType: "X_TEST", OpID: uuid.New(), OpStep: "POST"},
		JournalType: "X_TEST", Caller: "test",
		Lines: []domain.Line{{AccountCode: a, Direction: domain.Debit, Amount: amt}, {AccountCode: contract.AccountLoansPrincipal, Direction: domain.Credit, Amount: amt}},
	})
	if !errors.Is(err, domain.ErrInsufficientFunds) || retries.Load() != 0 {
		t.Fatalf("err=%v retries=%d", err, retries.Load())
	}
}
