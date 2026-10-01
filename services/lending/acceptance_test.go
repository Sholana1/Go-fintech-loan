package lending_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/lendingtest"
)

func accept(t testing.TB, env *lendingtest.Env, token, offerID, key string, body map[string]any) (int, []byte) {
	t.Helper()
	return env.Do(t, "POST", "/v1/loan-offers/"+offerID+"/accept", token, key, body)
}

func TestAcceptanceRequiresMatchingDisclosureAndStepUp(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	applicationID, offer := offered(t, env, c, amount100k, 3)

	// The hash proves which terms were accepted. A wrong hash is refused.
	bad := acceptBody(offer, c.PIN)
	bad["disclosure_hash"] = "0000000000000000000000000000000000000000000000000000000000000000"
	if st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), bad); st != 422 || lendingtest.ErrorCode(t, body) != "DISCLOSURE_MISMATCH" {
		t.Fatalf("wrong hash: %d %s", st, body)
	}
	// A wrong PIN is refused.
	if st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, "000001")); st != 403 || lendingtest.ErrorCode(t, body) != "STEP_UP_FAILED" {
		t.Fatalf("wrong PIN: %d %s", st, body)
	}
	// Missing idempotency key.
	if st, body := accept(t, env, c.Token, offer.OfferID, "", acceptBody(offer, c.PIN)); st != 400 {
		t.Fatalf("no key: %d %s", st, body)
	}
	// A third-party destination type is not something the API offers.
	weird := acceptBody(offer, c.PIN)
	weird["destination"] = map[string]any{"type": "THIRD_PARTY", "account_number": "0123456789"}
	if st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), weird); st != 400 {
		t.Fatalf("unknown destination: %d %s", st, body)
	}

	// None of the refused attempts changed anything.
	if a := getApplication(t, env, c, applicationID); a.Status != "OFFER_READY" {
		t.Fatalf("application moved after refused acceptances: %+v", a)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.loans`); n != 0 {
		t.Fatalf("%d loans exist after refused acceptances", n)
	}
	if posted, _ := balance(t, env, c); posted != 0 {
		t.Fatalf("customer balance %d after refused acceptances", posted)
	}

	// The correct acceptance works and its evidence is recorded.
	st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN))
	if st != http.StatusCreated {
		t.Fatalf("accept: %d %s", st, body)
	}
	var evidence []byte
	if err := env.Pool.QueryRow(ctx, `SELECT acceptance FROM lending.offers WHERE offer_id = $1`, offer.OfferID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{offer.DisclosureHash, "PIN_VERIFIED", "token_id"} {
		if !contains(evidence, want) {
			t.Fatalf("acceptance evidence %s lacks %q", evidence, want)
		}
	}
	if contains(evidence, c.PIN) {
		t.Fatal("the PIN was stored in the acceptance evidence")
	}
}

func TestExpiredOrSupersededOffersCannotBeAccepted(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})

	t.Run("expired by time, before the expiry job has run", func(t *testing.T) {
		c := env.Identity.Register(t)
		_, offer := offered(t, env, c, amount100k, 3)
		env.Clock.Advance(25 * time.Hour)
		login(t, env, &c)
		if st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN)); st != http.StatusGone || lendingtest.ErrorCode(t, body) != "OFFER_EXPIRED" {
			t.Fatalf("got %d %s", st, body)
		}
	})

	t.Run("expired by the expiry job", func(t *testing.T) {
		c := env.Identity.Register(t)
		applicationID, offer := offered(t, env, c, amount100k, 3)
		env.Clock.Advance(25 * time.Hour)
		if err := env.Service.ExpireStale(ctx); err != nil {
			t.Fatal(err)
		}
		login(t, env, &c)
		if st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN)); st != http.StatusGone {
			t.Fatalf("got %d %s", st, body)
		}
		if a := getApplication(t, env, c, applicationID); a.Status != "EXPIRED" {
			t.Fatalf("application: %+v", a)
		}
	})

	t.Run("voided when the application is cancelled", func(t *testing.T) {
		c := env.Identity.Register(t)
		applicationID, offer := offered(t, env, c, amount100k, 3)
		if st, body := env.Do(t, "POST", "/v1/loan-applications/"+applicationID+"/cancel", c.Token, "", nil); st != http.StatusOK {
			t.Fatalf("cancel: %d %s", st, body)
		}
		if st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN)); st != http.StatusConflict || lendingtest.ErrorCode(t, body) != "OFFER_NOT_OPEN" {
			t.Fatalf("got %d %s", st, body)
		}
	})

	t.Run("already accepted", func(t *testing.T) {
		c := env.Identity.Register(t)
		_, offer := offered(t, env, c, amount100k, 3)
		if st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN)); st != http.StatusCreated {
			t.Fatalf("first accept: %d %s", st, body)
		}
		if st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN)); st != http.StatusConflict {
			t.Fatalf("second accept with a new key: %d %s", st, body)
		}
	})

	if n := count(t, env, `SELECT count(*) FROM lending.loans`); n != 1 {
		t.Fatalf("%d loans exist, want exactly the one legitimately accepted", n)
	}
}

// Many simultaneous acceptances of one offer, each with its own idempotency
// key: one loan, one disbursement, one credit to the customer.
func TestConcurrentAcceptanceCreatesOneLoanAndOneDisbursement(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, offer := offered(t, env, c, amount100k, 3)

	var created, refused atomic.Int64
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN))
			switch st {
			case http.StatusCreated, http.StatusAccepted:
				created.Add(1)
			case http.StatusConflict, http.StatusGone:
				refused.Add(1)
			default:
				t.Errorf("unexpected %d %s", st, body)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 || refused.Load() != 11 {
		t.Fatalf("created=%d refused=%d, want 1/11", created.Load(), refused.Load())
	}
	if n := count(t, env, `SELECT count(*) FROM lending.loans`); n != 1 {
		t.Fatalf("%d loans", n)
	}
	if n := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_DISBURSEMENT'`); n != 1 {
		t.Fatalf("%d disbursement journals, want 1", n)
	}
	if posted, _ := balance(t, env, c); posted != offer.NetDisbursementMinor {
		t.Fatalf("customer credited %d, want %d exactly once", posted, offer.NetDisbursementMinor)
	}
	reconcileClean(t, env)
}

// The same acceptance retried with the same key (a client that timed out and
// tried again) returns the same loan and moves no more money.
func TestAcceptanceReplayReturnsTheSameLoan(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, offer := offered(t, env, c, amount100k, 3)
	key := lendingtest.Key()

	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body := accept(t, env, c.Token, offer.OfferID, key, acceptBody(offer, c.PIN))
			if st != http.StatusCreated && st != http.StatusAccepted {
				t.Errorf("accept %d: %d %s", i, st, body)
				return
			}
			ids[i] = lendingtest.Decode[loanView](t, body).LoanID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("replays returned different loans: %v", ids)
		}
	}
	// Same key, different content (auto-debit flipped) is refused.
	changed := acceptBody(offer, c.PIN)
	changed["auto_debit_authorised"] = true
	if st, body := accept(t, env, c.Token, offer.OfferID, key, changed); st != 422 || lendingtest.ErrorCode(t, body) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("same key, different body: %d %s", st, body)
	}
	if posted, _ := balance(t, env, c); posted != offer.NetDisbursementMinor {
		t.Fatalf("balance %d, want one disbursement of %d", posted, offer.NetDisbursementMinor)
	}
}

// Crash between "acceptance committed" and "ledger posted": the ledger is
// unreachable when the offer is accepted. The customer is told the loan is
// being disbursed, never that it is disbursed. When the ledger returns, the
// sweeper completes the disbursement, once.
func TestDisbursementSurvivesLedgerOutageAtAcceptance(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	applicationID, offer := offered(t, env, c, amount100k, 3)

	env.Faults.FailBefore("Post", 1)
	st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN))
	if st != http.StatusAccepted {
		t.Fatalf("accept during ledger outage: %d %s", st, body)
	}
	loan := lendingtest.Decode[loanView](t, body)
	if loan.Status != "DISBURSING" {
		t.Fatalf("loan must be reported as DISBURSING, not %s", loan.Status)
	}
	if a := getApplication(t, env, c, applicationID); a.Status != "DISBURSING" {
		t.Fatalf("application status %s", a.Status)
	}
	if posted, _ := balance(t, env, c); posted != 0 {
		t.Fatalf("customer has %d before the ledger posted", posted)
	}
	// The database itself refuses to call a loan active without a journal.
	if _, err := env.Pool.Exec(ctx, `UPDATE lending.loans SET state = 'ACTIVE' WHERE loan_id = $1`, loan.LoanID); err == nil {
		t.Fatal("a loan was marked ACTIVE without a disbursement journal")
	}

	// Recovery.
	env.Clock.Advance(time.Minute)
	if err := env.Service.ProcessDueIntents(ctx); err != nil {
		t.Fatal(err)
	}
	if l := getLoan(t, env, c, loan.LoanID); l.Status != "ACTIVE" {
		t.Fatalf("after recovery: %+v", l)
	}
	if posted, _ := balance(t, env, c); posted != offer.NetDisbursementMinor {
		t.Fatalf("balance %d after recovery", posted)
	}
	reconcileClean(t, env)
}

// The ledger posts the disbursement but lending never hears (lost response,
// or lending died before acting on it). Recovery must not post it again.
func TestDisbursementIsNotRepeatedAfterALostLedgerResponse(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, offer := offered(t, env, c, amount100k, 3)

	env.Faults.FailAfter("Post", 1)
	st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN))
	if st != http.StatusAccepted {
		t.Fatalf("accept: %d %s", st, body)
	}
	loan := lendingtest.Decode[loanView](t, body)
	// The money is already in the ledger; lending does not know yet.
	if posted, _ := balance(t, env, c); posted != offer.NetDisbursementMinor {
		t.Fatalf("ledger balance %d", posted)
	}
	if loan.Status != "DISBURSING" {
		t.Fatalf("lending status %s, want DISBURSING until it has confirmed the posting", loan.Status)
	}

	// The sweeper runs repeatedly (as it would across restarts).
	env.Clock.Advance(time.Minute)
	for range 3 {
		if err := env.Service.ProcessDueIntents(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if l := getLoan(t, env, c, loan.LoanID); l.Status != "ACTIVE" {
		t.Fatalf("after recovery: %+v", l)
	}
	if n := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_DISBURSEMENT'`); n != 1 {
		t.Fatalf("%d disbursement journals after recovery, want 1", n)
	}
	if posted, _ := balance(t, env, c); posted != offer.NetDisbursementMinor {
		t.Fatalf("balance %d: the disbursement was repeated", posted)
	}
	if env.Metrics.Count("duplicate:DISBURSEMENT") != 1 {
		t.Fatalf("the prevented duplicate was not counted: %d", env.Metrics.Count("duplicate:DISBURSEMENT"))
	}
	reconcileClean(t, env)
}

// The disbursement command issued many times at once (several workers, a
// sweeper and a request all at the same moment) produces one journal.
func TestDuplicateDisbursementCommandsPostOnce(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, offer := offered(t, env, c, amount100k, 3)

	env.Faults.FailBefore("Post", 1) // leave the intent pending
	st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN))
	if st != http.StatusAccepted {
		t.Fatalf("accept: %d %s", st, body)
	}
	var intentID uuid.UUID
	if err := env.Pool.QueryRow(ctx, `SELECT intent_id FROM lending.posting_intents WHERE kind = 'DISBURSEMENT'`).Scan(&intentID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := env.Service.ProcessIntent(ctx, intentID); err != nil {
				t.Errorf("process intent: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_DISBURSEMENT'`); n != 1 {
		t.Fatalf("%d disbursement journals, want 1", n)
	}
	if posted, _ := balance(t, env, c); posted != offer.NetDisbursementMinor {
		t.Fatalf("balance %d", posted)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.outbox WHERE event_type = 'loan.disbursed'`); n != 1 {
		t.Fatalf("%d loan.disbursed events, want 1", n)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.audit_log WHERE action = 'LOAN_DISBURSED'`); n != 1 {
		t.Fatalf("%d LOAN_DISBURSED audit rows, want 1", n)
	}
	reconcileClean(t, env)
}

// The ledger refuses the disbursement (the customer's account was frozen
// after the offer was made). The loan is not active, nothing is owed, and
// operations are alerted.
func TestRefusedDisbursementFailsTheLoanAndAlertsOperations(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	applicationID, offer := offered(t, env, c, amount100k, 3)

	owner, err := pgx.Connect(ctx, env.Ledger.DB.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	if _, err := owner.Exec(ctx, `UPDATE ledger.accounts SET status = 'FROZEN' WHERE code = $1`, c.AccountCode); err != nil {
		t.Fatal(err)
	}

	st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN))
	if st != http.StatusCreated {
		t.Fatalf("accept: %d %s", st, body)
	}
	loan := lendingtest.Decode[loanView](t, body)
	if loan.Status != "DISBURSEMENT_FAILED" {
		t.Fatalf("loan status %s", loan.Status)
	}
	if a := getApplication(t, env, c, applicationID); a.Status != "FAILED" {
		t.Fatalf("application status %s", a.Status)
	}
	if got := ledgerPosted(t, env, contract.AccountLoansPrincipal); got != 0 {
		t.Fatalf("a receivable of %d exists for a loan that was never disbursed", got)
	}
	open, err := env.Service.ReconExceptions(ctx, "OPEN")
	if err != nil || len(open) != 1 || open[0].Kind != "POSTING_REJECTED" {
		t.Fatalf("exceptions %+v err %v", open, err)
	}
	// The failed loan does not block a new application.
	submit(t, env, c, amount100k, 3, income300k)
}

// One customer can never read or act on another customer's application,
// offer or loan. The answer is the same as for something that does not exist.
func TestCustomersCannotReachEachOthersLoans(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	owner := env.Identity.Register(t)
	intruder := env.Identity.Register(t)

	applicationID, offer := offered(t, env, owner, amount100k, 3)

	// Before acceptance: the intruder tries to read and to accept.
	for _, path := range []string{"/v1/loan-applications/" + applicationID, "/v1/loan-offers/" + offer.OfferID} {
		if st, body := env.Do(t, "GET", path, intruder.Token, "", nil); st != http.StatusNotFound {
			t.Fatalf("intruder GET %s: %d %s", path, st, body)
		}
	}
	// Even with the correct disclosure hash and their own valid PIN.
	if st, body := accept(t, env, intruder.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, intruder.PIN)); st != http.StatusNotFound {
		t.Fatalf("intruder accept: %d %s", st, body)
	}
	if st, body := env.Do(t, "POST", "/v1/loan-applications/"+applicationID+"/cancel", intruder.Token, "", nil); st != http.StatusNotFound {
		t.Fatalf("intruder cancel: %d %s", st, body)
	}

	st, body := accept(t, env, owner.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, owner.PIN))
	if st != http.StatusCreated {
		t.Fatalf("owner accept: %d %s", st, body)
	}
	loanID := lendingtest.Decode[loanView](t, body).LoanID
	env.Ledger.Fund(t, intruder.AccountCode, 5_000_000)

	for _, path := range []string{"/v1/loans/" + loanID, "/v1/loans/" + loanID + "/statement", "/v1/loans/" + loanID + "/payoff-quote"} {
		if st, body := env.Do(t, "GET", path, intruder.Token, "", nil); st != http.StatusNotFound {
			t.Fatalf("intruder GET %s: %d %s", path, st, body)
		}
	}
	// The intruder cannot "repay" someone else's loan either (which would be
	// a way to probe loan ids, and to move money against a loan they do not own).
	if st, body := repay(t, env, intruder, loanID, 100_000); st != http.StatusNotFound {
		t.Fatalf("intruder repay: %d %s", st, body)
	}
	if posted, _ := balance(t, env, intruder); posted != 5_000_000 {
		t.Fatalf("intruder balance changed to %d", posted)
	}
	// The intruder's own loan list does not include it.
	st, body = env.Do(t, "GET", "/v1/loans", intruder.Token, "", nil)
	if st != http.StatusOK || contains(body, loanID) {
		t.Fatalf("intruder loan list: %d %s", st, body)
	}

	// Authentication and role boundaries.
	if st, _ := env.Do(t, "GET", "/v1/loans/"+loanID, "", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("no token: %d", st)
	}
	if st, _ := env.Do(t, "GET", "/v1/loans/"+loanID, "not-a-token", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("garbage token: %d", st)
	}
	_, staff := env.Identity.StaffToken(t, authn.RoleOpsViewer)
	if st, _ := env.Do(t, "GET", "/v1/loans/"+loanID, staff, "", nil); st != http.StatusForbidden {
		t.Fatalf("staff on a customer endpoint: %d", st)
	}
	if st, _ := env.Do(t, "GET", "/v1/ops/loans/"+loanID, owner.Token, "", nil); st != http.StatusForbidden {
		t.Fatalf("customer on a staff endpoint: %d", st)
	}
	if st, _ := env.Do(t, "GET", "/v1/ops/portfolio", owner.Token, "", nil); st != http.StatusForbidden {
		t.Fatalf("customer on portfolio: %d", st)
	}
	if st, body := env.Do(t, "GET", "/v1/ops/loans/"+loanID, staff, "", nil); st != http.StatusOK {
		t.Fatalf("staff viewer: %d %s", st, body)
	}
	// An expired token is refused.
	env.Clock.Advance(16 * time.Minute)
	if st, _ := env.Do(t, "GET", "/v1/loans/"+loanID, owner.Token, "", nil); st != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", st)
	}
}

// Backlog recovery: the ledger is unreachable while many customers accept
// their offers. Every acceptance is recorded durably and reported as
// "disbursing". When the ledger returns, the sweeper drains the whole
// backlog; each loan is disbursed exactly once.
func TestBacklogOfPendingDisbursementsDrainsAfterAnOutage(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	const n = 40

	type pending struct {
		accountCode string
		net         int64
	}
	var loans []pending
	for range n {
		c := env.Identity.Register(t)
		_, offer := offered(t, env, c, amount100k, 3)
		env.Faults.FailBefore("Post", 1)
		st, body := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c.PIN))
		if st != http.StatusAccepted {
			t.Fatalf("accept during outage: %d %s", st, body)
		}
		loans = append(loans, pending{c.AccountCode, offer.NetDisbursementMinor})
	}
	if got := count(t, env, `SELECT count(*) FROM lending.posting_intents WHERE state = 'PENDING' AND kind = 'DISBURSEMENT'`); got != n {
		t.Fatalf("%d pending disbursements, want %d", got, n)
	}
	gauges, err := env.Store.ReadGauges(ctx, env.Clock.Now())
	if err != nil || gauges.PendingDisbursements != n {
		t.Fatalf("pending-disbursement gauge %d err %v", gauges.PendingDisbursements, err)
	}

	// The ledger is back. Several sweeper instances run at once.
	env.Clock.Advance(time.Minute)
	started := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := env.Service.ProcessDueIntents(ctx); err != nil {
				t.Errorf("sweeper: %v", err)
			}
		}()
	}
	wg.Wait()
	t.Logf("drained %d pending disbursements with 4 concurrent sweepers in %s", n, time.Since(started).Round(time.Millisecond))

	if got := count(t, env, `SELECT count(*) FROM lending.posting_intents WHERE state = 'PENDING'`); got != 0 {
		t.Fatalf("%d intents still pending after the drain", got)
	}
	if got := count(t, env, `SELECT count(*) FROM lending.loans WHERE state = 'ACTIVE'`); got != n {
		t.Fatalf("%d active loans, want %d", got, n)
	}
	if got := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_DISBURSEMENT'`); got != n {
		t.Fatalf("%d disbursement journals, want %d (one per loan)", got, n)
	}
	for _, l := range loans {
		if posted, _ := env.Ledger.Balance(t, l.accountCode); posted != l.net {
			t.Fatalf("account %s holds %d, want %d", l.accountCode, posted, l.net)
		}
	}
	reconcileClean(t, env)
}
