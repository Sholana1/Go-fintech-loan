package ledger_test

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/ledger/ledgertest"
)

// The database itself rejects an unbalanced journal at COMMIT, even when the
// application layer is bypassed entirely.
func TestDatabaseRejectsUnbalancedJournalAtCommit(t *testing.T) {
	env := ledgertest.Start(t)
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 10_000)

	tx, err := env.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	var journalID int64
	var postedAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO ledger.journals (business_date, journal_type, op_type, op_id, op_step, created_by)
		VALUES (current_date, 'BAD', 'TEST', gen_random_uuid(), 'POST', 'test')
		RETURNING journal_id, posted_at`).Scan(&journalID, &postedAt); err != nil {
		t.Fatal(err)
	}
	// Each row passes every row-level CHECK. Only the set is wrong.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger.entries (journal_id, posted_at, account_id, currency, direction, amount, balance_after)
		SELECT $1, $2, account_id, 'NGN', d::ledger.side, amt, 0
		  FROM (VALUES ($3, 'DEBIT', 100::bigint), ('SYS:LOANS_PRINCIPAL', 'CREDIT', 99::bigint)) v(code, d, amt)
		  JOIN ledger.accounts USING (code)`, journalID, postedAt, a); err != nil {
		t.Fatalf("row-level constraints unexpectedly rejected the rows: %v", err)
	}
	err = tx.Commit(ctx)
	if pgxutil.Constraint(err) != "journal_balanced" {
		t.Fatalf("commit should fail on journal_balanced, got %v", err)
	}

	if posted, _ := env.Balance(t, a); posted != 10_000 {
		t.Fatalf("balance changed by a rejected journal: %d", posted)
	}
	env.AssertInvariants(t)
}

func TestPostedRowsAreImmutableAndBalancesAreNotWritable(t *testing.T) {
	env := ledgertest.Start(t)
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 10_000)

	owner, err := pgx.Connect(ctx, env.DB.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)

	// Even the schema owner is stopped by the immutability triggers.
	for _, stmt := range []string{
		`UPDATE ledger.entries SET amount = 1`,
		`DELETE FROM ledger.entries`,
		`UPDATE ledger.journals SET narrative = 'edited'`,
		`DELETE FROM ledger.journals`,
		`TRUNCATE ledger.entries`,
	} {
		if _, err := owner.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%s: want immutability error, got %v", stmt, err)
		}
	}

	// The application role cannot edit a balance or rewrite history.
	for _, stmt := range []string{
		`UPDATE ledger.balances SET posted = 999999999`,
		`UPDATE ledger.balances SET held = 0`,
		`UPDATE ledger.balances SET floor = NULL`,
		`UPDATE ledger.entries SET amount = 1`,
		`DELETE FROM ledger.posting_refs`,
		`INSERT INTO ledger.posting_rights VALUES ('lending', 'DEV_FUNDING')`,
	} {
		if _, err := env.Pool.Exec(ctx, stmt); pgxutil.Code(err) != "42501" {
			t.Errorf("%s: want permission denied (42501), got %v", stmt, err)
		}
	}
	env.AssertInvariants(t)
}

// Connections are killed while postings are in flight. Afterwards every
// operation is retried with its original reference. Each must end up posted
// exactly once, and the ledger must still satisfy every invariant.
func TestInterruptedTransactionsLeaveNoPartialPostings(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	const n, amount = 120, 1_000
	env.Fund(t, a, n*amount)

	refs := make([]*ledgerv1.PostingRef, n)
	for i := range refs {
		refs[i] = newRef("X_TEST")
	}

	admin, err := pgx.Connect(ctx, env.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	stop := make(chan struct{})
	var killed atomic.Int64
	var killer sync.WaitGroup
	killer.Add(1)
	go func() {
		defer killer.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(12 * time.Millisecond):
				var k int64
				_ = admin.QueryRow(ctx, `
					SELECT count(*) FILTER (WHERE pg_terminate_backend(pid))
					  FROM pg_stat_activity
					 WHERE datname = current_database() AND application_name = 'ledger-test' AND state <> 'idle'`).Scan(&k)
				killed.Add(k)
			}
		}
	}()

	var failures atomic.Int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, ref := range refs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := lending.PostJournal(ctx, repayment(ref, a, amount)); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	close(stop)
	killer.Wait()
	t.Logf("terminated %d backends; %d of %d first attempts failed", killed.Load(), failures.Load(), n)
	if killed.Load() == 0 {
		t.Log("no backend was caught mid-transaction on this run; the retry path below is still exercised")
	}

	// Recovery: retry everything with the same references.
	for _, ref := range refs {
		var lastErr error
		for range 5 {
			if _, lastErr = lending.PostJournal(ctx, repayment(ref, a, amount)); lastErr == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if lastErr != nil {
			t.Fatalf("retry failed: %v", lastErr)
		}
	}

	if posted, _ := env.Balance(t, a); posted != 0 {
		t.Fatalf("balance %d, want 0: each of %d operations must be debited exactly once", posted, n)
	}
	var journals int
	if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger.journals WHERE op_type = 'X_TEST'`).Scan(&journals); err != nil {
		t.Fatal(err)
	}
	if journals != n {
		t.Fatalf("%d journals, want %d", journals, n)
	}
	env.AssertInvariants(t)
}

// Property test: a long random mix of postings, holds, captures and releases
// across several accounts, run concurrently, including many that must be
// refused. Whatever happens, the invariants must hold at the end and no
// customer balance may be negative.
func TestRandomOperationsPreserveInvariants(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	ctx := context.Background()

	const accounts = 6
	codes := make([]string, accounts)
	for i := range codes {
		_, codes[i] = env.OpenCustomer(t)
		env.Fund(t, codes[i], 200_000)
	}

	seed := uint64(time.Now().UnixNano())
	t.Logf("seed %d", seed)

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w)))
			var openHolds []*ledgerv1.HoldRef
			for range 150 {
				acct := codes[rng.IntN(accounts)]
				amount := int64(1 + rng.IntN(60_000))
				switch rng.IntN(5) {
				case 0, 1: // debit, often more than is available
					_, err := lending.PostJournal(ctx, repayment(newRef("X_TEST"), acct, amount))
					expectOKOrFunds(t, err)
				case 2: // credit back
					_, err := lending.PostJournal(ctx, &ledgerv1.PostJournalRequest{
						Ref: newRef("X_TEST"), JournalType: contract.JournalLoanDisbursement,
						Lines: []*ledgerv1.Line{line(contract.AccountLoansPrincipal, debit, amount), line(acct, credit, amount)},
					})
					if err != nil {
						t.Errorf("credit: %v", err)
					}
				case 3: // hold
					ref := &ledgerv1.HoldRef{OpType: contract.HoldLoanPayout, OpId: uuid.NewString()}
					_, err := lending.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{Ref: ref, AccountCode: acct, Amount: ngn(amount)})
					if expectOKOrFunds(t, err) {
						openHolds = append(openHolds, ref)
					}
				case 4: // close a hold, by capture or release
					if len(openHolds) == 0 {
						continue
					}
					ref := openHolds[len(openHolds)-1]
					openHolds = openHolds[:len(openHolds)-1]
					if rng.IntN(2) == 0 {
						if _, err := lending.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{Hold: ref}); err != nil {
							t.Errorf("release: %v", err)
						}
						continue
					}
					// Capture 1 kobo: always within the hold, on whichever
					// account the hold is on (looked up from the database).
					var code string
					if err := env.Pool.QueryRow(ctx, `
						SELECT a.code FROM ledger.holds h JOIN ledger.accounts a USING (account_id)
						 WHERE h.op_type = $1 AND h.op_id = $2`, ref.GetOpType(), ref.GetOpId()).Scan(&code); err != nil {
						t.Errorf("lookup hold: %v", err)
						continue
					}
					if _, err := lending.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{
						Hold: ref, Ref: newRef("X_CAPTURE"), JournalType: contract.JournalPayoutCapture,
						Lines: []*ledgerv1.Line{line(code, debit, 1), line(contract.AccountPayoutClearing, credit, 1)},
					}); err != nil {
						t.Errorf("capture: %v", err)
					}
				}
			}
		}()
	}
	wg.Wait()

	var negative int
	if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger.balances WHERE floor IS NOT NULL AND posted - held < floor`).Scan(&negative); err != nil {
		t.Fatal(err)
	}
	if negative != 0 {
		t.Fatalf("%d customer accounts are below their floor", negative)
	}
	env.AssertInvariants(t)
}
