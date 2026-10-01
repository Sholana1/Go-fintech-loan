package ledger_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	commonv1 "bankplatform.internal/gen/bank/common/v1"
	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/ledger/ledgertest"
)

func TestAuthorisationIsByWorkloadAndJournalShape(t *testing.T) {
	env := ledgertest.Start(t)
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	_, b := env.OpenCustomer(t)
	env.Fund(t, a, 100_000)

	lending := env.Client(t, "lending")
	identity := env.Client(t, "identity")
	stranger := env.Client(t, "unknown-service")

	// identity may open accounts but not post.
	_, err := identity.PostJournal(ctx, repayment(newRef("X_TEST"), a, 100))
	wantReason(t, err, codes.PermissionDenied, contract.ReasonNotAuthorised)
	// A workload with no rights can do nothing, including read.
	_, err = stranger.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountCode: a})
	wantReason(t, err, codes.PermissionDenied, contract.ReasonNotAuthorised)
	// lending may not open accounts or use journal types it was not granted.
	_, err = lending.OpenAccount(ctx, &ledgerv1.OpenAccountRequest{Code: "CUST:" + uuid.NewString() + ":MAIN", Kind: ledgerv1.AccountKind_ACCOUNT_KIND_CUSTOMER_DEPOSIT, Currency: "NGN", OwnerId: uuid.NewString()})
	wantReason(t, err, codes.PermissionDenied, contract.ReasonNotAuthorised)
	_, err = lending.PostJournal(ctx, &ledgerv1.PostJournalRequest{
		Ref: newRef("X_TEST"), JournalType: "DEV_FUNDING",
		Lines: []*ledgerv1.Line{line("SYS:DEV_FUNDING", debit, 100), line(a, credit, 100)},
	})
	wantReason(t, err, codes.PermissionDenied, contract.ReasonNotAuthorised)

	// Even with a valid right, a journal type cannot be bent into moving
	// money between two customers...
	_, err = lending.PostJournal(ctx, &ledgerv1.PostJournalRequest{
		Ref: newRef("X_TEST"), JournalType: contract.JournalLoanRepayment,
		Lines: []*ledgerv1.Line{line(a, debit, 100), line(b, credit, 100)},
	})
	wantReason(t, err, codes.PermissionDenied, contract.ReasonNotAuthorised)
	// ...or into touching a system account outside its policy.
	_, err = lending.PostJournal(ctx, &ledgerv1.PostJournalRequest{
		Ref: newRef("X_TEST"), JournalType: contract.JournalLoanRepayment,
		Lines: []*ledgerv1.Line{line(a, debit, 100), line(contract.AccountCashSettlementBank, credit, 100)},
	})
	wantReason(t, err, codes.PermissionDenied, contract.ReasonNotAuthorised)

	if pa, _ := env.Balance(t, a); pa != 100_000 {
		t.Fatalf("a=%d, want untouched 100000", pa)
	}
	if pb, _ := env.Balance(t, b); pb != 0 {
		t.Fatalf("b=%d, want untouched 0", pb)
	}
}

func TestRequestValidationAndAccountStatus(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 100_000)

	// Unbalanced lines are rejected before anything is locked.
	_, err := lending.PostJournal(ctx, &ledgerv1.PostJournalRequest{
		Ref: newRef("X_TEST"), JournalType: contract.JournalLoanRepayment,
		Lines: []*ledgerv1.Line{line(a, debit, 100), line(contract.AccountLoansPrincipal, credit, 99)},
	})
	wantReason(t, err, codes.InvalidArgument, contract.ReasonUnbalanced)

	// Unknown account.
	ghost := "CUST:" + uuid.NewString() + ":MAIN"
	_, err = lending.PostJournal(ctx, repayment(newRef("X_TEST"), ghost, 100))
	wantReason(t, err, codes.NotFound, contract.ReasonAccountNotFound)

	// Wrong currency for the account.
	_, err = lending.PostJournal(ctx, &ledgerv1.PostJournalRequest{
		Ref: newRef("X_TEST"), JournalType: contract.JournalLoanRepayment,
		Lines: []*ledgerv1.Line{
			{AccountCode: a, Direction: debit, Amount: &commonv1.Money{MinorUnits: 100, Currency: "USD"}},
			{AccountCode: contract.AccountLoansPrincipal, Direction: credit, Amount: &commonv1.Money{MinorUnits: 100, Currency: "USD"}},
		},
	})
	wantReason(t, err, codes.InvalidArgument, contract.ReasonCurrencyMismatch)

	// A future business date is refused.
	req := repayment(newRef("X_TEST"), a, 100)
	req.BusinessDate = time.Now().AddDate(0, 0, 3).Format(time.DateOnly)
	_, err = lending.PostJournal(ctx, req)
	wantReason(t, err, codes.InvalidArgument, contract.ReasonInvalid)

	owner, err := pgx.Connect(ctx, env.DB.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	setStatus := func(s string) {
		if _, err := owner.Exec(ctx, `UPDATE ledger.accounts SET status = $1 WHERE code = $2`, s, a); err != nil {
			t.Fatal(err)
		}
	}

	// Post-no-debit: debits refused, credits accepted.
	setStatus("POST_NO_DEBIT")
	_, err = lending.PostJournal(ctx, repayment(newRef("X_TEST"), a, 100))
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonAccountNotActive)
	env.Fund(t, a, 500)

	// Frozen: nothing posts.
	setStatus("FROZEN")
	_, err = env.Client(t, "devtools").PostJournal(ctx, &ledgerv1.PostJournalRequest{
		Ref: newRef("DEV_FUNDING"), JournalType: "DEV_FUNDING",
		Lines: []*ledgerv1.Line{line("SYS:DEV_FUNDING", debit, 100), line(a, credit, 100)},
	})
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonAccountNotActive)

	if posted, _ := env.Balance(t, a); posted != 100_500 {
		t.Fatalf("balance %d, want 100500", posted)
	}
	env.AssertInvariants(t)
}

func TestGetJournalAndBalance(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 25_000)

	ref := newRef("X_TEST")
	posted, err := lending.PostJournal(ctx, repayment(ref, a, 5_000))
	if err != nil {
		t.Fatal(err)
	}
	j, err := lending.GetJournal(ctx, &ledgerv1.GetJournalRequest{Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if j.GetJournalId() != posted.GetJournalId() || j.GetCreatedBy() != "lending" || len(j.GetLines()) != 2 {
		t.Fatalf("unexpected journal %+v", j)
	}
	_, err = lending.GetJournal(ctx, &ledgerv1.GetJournalRequest{Ref: newRef("X_TEST")})
	wantReason(t, err, codes.NotFound, contract.ReasonJournalNotFound)

	bal, err := lending.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountCode: a})
	if err != nil || bal.GetPosted().GetMinorUnits() != 20_000 || bal.GetAvailable().GetMinorUnits() != 20_000 {
		t.Fatalf("balance %+v err %v", bal, err)
	}

	// Opening the same account again is idempotent; a conflicting owner is not.
	identity := env.Client(t, "identity")
	owner := strings.Split(a, ":")[1]
	again, err := identity.OpenAccount(ctx, &ledgerv1.OpenAccountRequest{Code: a, Kind: ledgerv1.AccountKind_ACCOUNT_KIND_CUSTOMER_DEPOSIT, Currency: "NGN", OwnerId: owner})
	if err != nil || !again.GetAlreadyExisted() {
		t.Fatalf("reopen: %v %+v", err, again)
	}
	_, err = identity.OpenAccount(ctx, &ledgerv1.OpenAccountRequest{Code: a, Kind: ledgerv1.AccountKind_ACCOUNT_KIND_CUSTOMER_DEPOSIT, Currency: "NGN", OwnerId: uuid.NewString()})
	wantReason(t, err, codes.FailedPrecondition, contract.ReasonAccountConflict)
}

// Every posting writes exactly one outbox row in the same transaction, and a
// rejected posting writes none.
func TestOutboxRowIsWrittenAtomicallyWithTheJournal(t *testing.T) {
	env := ledgertest.Start(t)
	lending := env.Client(t, "lending")
	ctx := context.Background()
	_, a := env.OpenCustomer(t)
	env.Fund(t, a, 1_000)

	count := func() (n int) {
		if err := env.Pool.QueryRow(ctx, `SELECT count(*) FROM ledger.outbox`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()

	ref := newRef("X_TEST")
	if _, err := lending.PostJournal(ctx, repayment(ref, a, 400)); err != nil {
		t.Fatal(err)
	}
	if _, err := lending.PostJournal(ctx, repayment(ref, a, 400)); err != nil { // duplicate
		t.Fatal(err)
	}
	if _, err := lending.PostJournal(ctx, repayment(newRef("X_TEST"), a, 5_000)); err == nil { // insufficient
		t.Fatal("expected insufficient funds")
	}
	if got := count() - before; got != 1 {
		t.Fatalf("%d outbox rows written, want exactly 1", got)
	}

	var eventType, key string
	var payload []byte
	if err := env.Pool.QueryRow(ctx, `SELECT event_type, partition_key, payload FROM ledger.outbox ORDER BY outbox_id DESC LIMIT 1`).
		Scan(&eventType, &key, &payload); err != nil {
		t.Fatal(err)
	}
	if eventType != contract.EventJournalPosted || key != ref.GetOpId() || !strings.Contains(string(payload), a) {
		t.Fatalf("unexpected outbox row: %s %s %s", eventType, key, payload)
	}
}
