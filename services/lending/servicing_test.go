package lending_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/lendingtest"
)

// Running the accrual job twice, or from several instances at the same time,
// recognises each day's interest exactly once.
func TestInterestAccrualIsIdempotentAndSafeToRunConcurrently(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	var customers = 6
	for range customers {
		c := env.Identity.Register(t)
		disbursed(t, env, c, amount100k, 3, false)
	}
	start := today(env)
	advanceTo(t, env, start.AddDate(0, 0, 1))

	// Eight concurrent runs for the same business date.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := env.Service.AccrueInterest(ctx, start); err != nil {
				t.Errorf("accrue: %v", err)
			}
		}()
	}
	wg.Wait()

	// One day of a 31- or 30-day first period on 400,000 interest per loan.
	periodDays := int64(bizdate.Days(start, bizdate.AddMonths(start, 1)))
	perLoan := int64(400_000) / periodDays
	want := perLoan * int64(customers)
	if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != want {
		t.Fatalf("interest receivable %d after concurrent runs, want %d", got, want)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.interest_accruals WHERE business_date = $1`, start); n != customers {
		t.Fatalf("%d accrual rows, want %d", n, customers)
	}
	if n := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_INTEREST_ACCRUAL'`); n != 1 {
		t.Fatalf("%d accrual journals for one date, want 1", n)
	}
	// And again, later: still nothing new.
	if err := env.Service.AccrueInterest(ctx, start); err != nil {
		t.Fatal(err)
	}
	if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != want {
		t.Fatalf("a repeat run changed the receivable to %d", got)
	}
	// Accrual for a date that is not over is refused.
	if err := env.Service.AccrueInterest(ctx, today(env)); err == nil {
		t.Fatal("accrual for the current, incomplete business date must be refused")
	}
	reconcileClean(t, env)
}

// The job did not run for nine days. When it resumes it processes each
// missed date in order and ends exactly where a daily run would have.
func TestMissedAccrualDaysAreCaughtUpInOrder(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	start := today(env)

	// Day 1: the job runs normally.
	advanceTo(t, env, start.AddDate(0, 0, 1))
	dailyJobs(t, env)
	// Then nothing for nine days.
	advanceTo(t, env, start.AddDate(0, 0, 10))
	dailyJobs(t, env)

	periodDays := int64(bizdate.Days(start, bizdate.AddMonths(start, 1)))
	want := int64(400_000) * 10 / periodDays
	if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != want {
		t.Fatalf("interest receivable %d after catch-up, want %d", got, want)
	}
	// One run per business date, none skipped.
	if n := count(t, env, `SELECT count(*) FROM lending.accrual_runs`); n != 10 {
		t.Fatalf("%d accrual runs, want one for each of 10 dates", n)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.interest_accruals WHERE loan_id = $1`, loanID); n != 10 {
		t.Fatalf("%d accrual rows for the loan, want 10", n)
	}
	reconcileClean(t, env)
}

// If the loan book and the ledger are made to disagree, reconciliation says
// so. It does not correct anything by itself.
func TestReconciliationDetectsDrift(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	reconcileClean(t, env)

	// Corrupt the loan book: mark 1,000 kobo of principal as paid with no
	// corresponding journal (the kind of damage a bad manual fix would do).
	owner, err := pgx.Connect(ctx, env.DB.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	if _, err := owner.Exec(ctx, `UPDATE lending.instalments SET principal_paid = 1000 WHERE loan_id = $1 AND seq = 1`, loanID); err != nil {
		t.Fatal(err)
	}

	res, err := env.Service.ReconcileLedger(ctx, time.Time{})
	if err != nil || res.Skipped || res.ControlBreaks != 1 {
		t.Fatalf("reconcile after corruption: %+v %v", res, err)
	}
	open, _ := env.Service.ReconExceptions(ctx, "OPEN")
	if len(open) != 1 || open[0].Kind != "CONTROL_ACCOUNT_MISMATCH" || open[0].EntityID != contract.AccountLoansPrincipal || open[0].AmountMinor != 1000 {
		t.Fatalf("exceptions %+v", open)
	}
	// Running it again does not duplicate the exception.
	if _, err := env.Service.ReconcileLedger(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if open, _ := env.Service.ReconExceptions(ctx, "OPEN"); len(open) != 1 {
		t.Fatalf("%d open exceptions after a second pass, want 1", len(open))
	}
	// Once the book is right again the exception is closed by the system.
	if _, err := owner.Exec(ctx, `UPDATE lending.instalments SET principal_paid = 0 WHERE loan_id = $1 AND seq = 1`, loanID); err != nil {
		t.Fatal(err)
	}
	reconcileClean(t, env)
	if open, _ := env.Service.ReconExceptions(ctx, "OPEN"); len(open) != 0 {
		t.Fatalf("exception still open after the book was corrected: %+v", open)
	}

	// Reconciliation refuses to compare while a posting is in flight: the
	// two sides legitimately differ at that moment.
	c2 := env.Identity.Register(t)
	_, offer := offered(t, env, c2, amount100k, 3)
	env.Faults.FailBefore("Post", 1)
	if st, body := accept(t, env, c2.Token, offer.OfferID, lendingtest.Key(), acceptBody(offer, c2.PIN)); st != 202 {
		t.Fatalf("accept: %d %s", st, body)
	}
	res, err = env.Service.ReconcileLedger(ctx, time.Time{})
	if err != nil || !res.Skipped {
		t.Fatalf("reconcile with a posting in flight: %+v %v", res, err)
	}
	if open, _ := env.Service.ReconExceptions(ctx, "OPEN"); len(open) != 0 {
		t.Fatalf("a posting in flight was reported as a break: %+v", open)
	}
}

func TestArrearsLateFeeAndClassification(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	loan := getLoan(t, env, c, loanID)
	due := firstDue(t, loan)

	// On the due date the instalment is due, not overdue.
	advanceTo(t, env, due)
	dailyJobs(t, env)
	login(t, env, &c)
	if l := getLoan(t, env, c, loanID); l.Status != "ACTIVE" || l.DaysPastDue != 0 {
		t.Fatalf("on the due date: %+v", l)
	}

	// Three days later: overdue, within the late-fee grace period.
	advanceTo(t, env, due.AddDate(0, 0, 3))
	dailyJobs(t, env)
	login(t, env, &c)
	l := getLoan(t, env, c, loanID)
	if l.Status != "OVERDUE" || l.DaysPastDue != 3 || l.ArrearsBucket != "DPD_1_30" || l.Instalments[0].FeesMinor != 0 {
		t.Fatalf("3 days past due: %+v", l)
	}

	// Four days: the late fee is charged, once.
	advanceTo(t, env, due.AddDate(0, 0, 4))
	dailyJobs(t, env)
	dailyJobs(t, env) // a second run the same day must not charge again
	if _, err := env.Service.ClassifyArrears(ctx, today(env)); err != nil {
		t.Fatal(err)
	}
	login(t, env, &c)
	l = getLoan(t, env, c, loanID)
	if l.Instalments[0].FeesMinor != env.Product.LateFeeMinor {
		t.Fatalf("late fee %d, want %d charged exactly once", l.Instalments[0].FeesMinor, env.Product.LateFeeMinor)
	}
	if got := ledgerPosted(t, env, contract.AccountLoansFeesReceivable); got != env.Product.LateFeeMinor {
		t.Fatalf("fees receivable %d", got)
	}
	reconcileClean(t, env)

	// Paying now clears the fee first, then interest, then principal.
	st, body := repay(t, env, c, loanID, 500_000)
	rep := lendingtest.Decode[repaymentView](t, body)
	if st != http.StatusCreated || rep.FeesMinor != 50_000 || rep.InterestMinor != 400_000 || rep.PrincipalMinor != 50_000 {
		t.Fatalf("repayment while overdue: %d %+v", st, rep)
	}
	// Clearing the arrears returns the loan to ACTIVE immediately.
	st, body = repay(t, env, c, loanID, loan.Instalments[0].PrincipalMinor-50_000)
	if st != http.StatusCreated {
		t.Fatalf("clear arrears: %d %s", st, body)
	}
	if l := getLoan(t, env, c, loanID); l.Status != "ACTIVE" || l.DaysPastDue != 0 || l.ArrearsBucket != "CURRENT" {
		t.Fatalf("after clearing arrears: %+v", l)
	}
	reconcileClean(t, env)
}

// Scheduled collection: the customer authorised debits of their own account
// on due dates. The job takes what is due, never more, retries a bounded
// number of times, and takes a partial amount when that is all there is.
func TestScheduledCollection(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, true)
	loan := getLoan(t, env, c, loanID)
	due := firstDue(t, loan)
	instalment := loan.Instalments[0].PrincipalMinor + loan.Instalments[0].InterestMinor

	collect := func() {
		t.Helper()
		if err := env.Service.CollectDue(ctx); err != nil {
			t.Fatalf("collect: %v", err)
		}
	}

	// Before the due date nothing is collected, however much is available.
	advanceTo(t, env, due.AddDate(0, 0, -1))
	collect()
	if posted, _ := balance(t, env, c); posted != 9_900_000 {
		t.Fatalf("collected before the due date: balance %d", posted)
	}

	// On the due date exactly the instalment is taken; the rest stays.
	advanceTo(t, env, due)
	dailyJobs(t, env)
	collect()
	if posted, _ := balance(t, env, c); posted != 9_900_000-instalment {
		t.Fatalf("balance %d after collection, want %d", posted, 9_900_000-instalment)
	}
	login(t, env, &c)
	if !getLoan(t, env, c, loanID).Instalments[0].Paid {
		t.Fatal("first instalment not paid by collection")
	}
	// A second run the same day takes nothing more (no paying ahead).
	collect()
	if posted, _ := balance(t, env, c); posted != 9_900_000-instalment {
		t.Fatalf("collection took more than was due: balance %d", posted)
	}
	var source string
	if err := env.Pool.QueryRow(ctx, `SELECT source FROM lending.repayments WHERE loan_id = $1`, loanID).Scan(&source); err != nil || source != "AUTO_DEBIT" {
		t.Fatalf("repayment source %q err %v", source, err)
	}
	reconcileClean(t, env)
}

func TestCollectionTakesPartialAmountsAndIsBounded(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	// A single-instalment loan: 100,000 + 4% falls due in one month.
	loanID := disbursed(t, env, c, amount100k, 1, true)
	loan := getLoan(t, env, c, loanID)
	due := firstDue(t, loan)

	advanceTo(t, env, due)
	dailyJobs(t, env)
	// Only 9,900,000 is available against 10,400,000 due: take what is there.
	if err := env.Service.CollectDue(ctx); err != nil {
		t.Fatal(err)
	}
	if posted, _ := balance(t, env, c); posted != 0 {
		t.Fatalf("balance %d after partial collection, want 0", posted)
	}
	login(t, env, &c)
	inst := getLoan(t, env, c, loanID).Instalments[0]
	if inst.InterestPaid != 400_000 || inst.PrincipalPaid != 9_500_000 || inst.Paid {
		t.Fatalf("after partial collection: %+v", inst)
	}

	// With nothing in the account, retries happen at most
	// CollectionAttemptsPerDay times and then wait for tomorrow.
	retry := time.Duration(env.Product.CollectionRetryAfterHours)*time.Hour + time.Minute
	for range env.Product.CollectionAttemptsPerDay + 3 {
		env.Clock.Advance(retry / 4)
		if err := env.Service.CollectDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var attempts int
	var next time.Time
	if err := env.Pool.QueryRow(ctx, `SELECT collection_attempts, next_collection_at FROM lending.loans WHERE loan_id = $1`, loanID).Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.repayments WHERE loan_id = $1`, loanID); n != 1 {
		t.Fatalf("%d repayments recorded, want only the one that took money", n)
	}
	if !next.After(env.Clock.Now()) {
		t.Fatalf("next collection %s is not in the future", next)
	}

	// Money arrives: the next collection takes the remainder and closes the loan.
	env.Ledger.Fund(t, c.AccountCode, 1_000_000)
	env.Clock.Advance(next.Sub(env.Clock.Now()) + time.Minute)
	dailyJobs(t, env)
	if err := env.Service.CollectDue(ctx); err != nil {
		t.Fatal(err)
	}
	login(t, env, &c)
	if l := getLoan(t, env, c, loanID); l.Status != "CLOSED" {
		t.Fatalf("loan after final collection: %+v", l)
	}
	reconcileClean(t, env)
}

func TestWriteOffRequiresMakerCheckerAndRecoveriesAreTracked(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 1, false)
	loan := getLoan(t, env, c, loanID)
	due := firstDue(t, loan)

	_, maker := env.Identity.StaffToken(t, authn.RoleOpsMaker)

	propose := func(token string) (int, []byte) {
		return env.Do(t, "POST", "/v1/ops/admin-actions", token, "", map[string]any{
			"kind": "WRITE_OFF", "loan_id": loanID, "reason": "customer deceased; estate has no assets"})
	}

	// Too early: the loan is not in arrears at all.
	if st, body := propose(maker); st != http.StatusConflict {
		t.Fatalf("write-off of a current loan: %d %s", st, body)
	}
	// 30 days past due: in arrears but below the product threshold.
	advanceTo(t, env, due.AddDate(0, 0, 30))
	dailyJobs(t, env)
	_, maker = relogin(t, env, authn.RoleOpsMaker)
	if st, body := propose(maker); st != http.StatusForbidden {
		t.Fatalf("write-off below the days-past-due threshold: %d %s", st, body)
	}

	advanceTo(t, env, due.AddDate(0, 0, 95))
	dailyJobs(t, env)
	// Staff tokens expire as the test moves the clock: sign in afresh.
	_, maker = relogin(t, env, authn.RoleOpsMaker)
	_, checker := relogin(t, env, authn.RoleOpsChecker)
	_, viewer := relogin(t, env, authn.RoleOpsViewer)
	_, both := relogin(t, env, authn.RoleOpsMaker, authn.RoleOpsChecker)

	// Roles: a viewer cannot propose; a maker cannot approve.
	if st, _ := propose(viewer); st != http.StatusForbidden {
		t.Fatalf("viewer proposing: %d", st)
	}
	st, body := propose(both)
	if st != http.StatusCreated {
		t.Fatalf("propose: %d %s", st, body)
	}
	actionID := lendingtest.Decode[struct {
		ActionID string `json:"action_id"`
	}](t, body).ActionID
	// A second open action for the same loan is refused.
	if st, _ := propose(maker); st != http.StatusConflict {
		t.Fatalf("second open action: %d", st)
	}
	if st, _ := env.Do(t, "POST", "/v1/ops/admin-actions/"+actionID+"/approve", maker, "", map[string]any{"note": "ok"}); st != http.StatusForbidden {
		t.Fatalf("maker role approving: %d", st)
	}
	// The same person holding both roles still cannot approve their own proposal.
	if st, body := env.Do(t, "POST", "/v1/ops/admin-actions/"+actionID+"/approve", both, "", map[string]any{"note": "approving my own"}); st != http.StatusForbidden {
		t.Fatalf("self-approval: %d %s", st, body)
	}
	// The database refuses it too, independently of the application.
	if _, err := env.Pool.Exec(ctx, `UPDATE lending.admin_actions SET checker_id = maker_id WHERE action_id = $1`, actionID); err == nil {
		t.Fatal("the database allowed checker = maker")
	}
	// Nothing has happened to the loan yet.
	if got := ledgerPosted(t, env, contract.AccountLoanWriteOffExpense); got != 0 {
		t.Fatalf("write-off expense %d before approval", got)
	}

	// A different person approves.
	st, body = env.Do(t, "POST", "/v1/ops/admin-actions/"+actionID+"/approve", checker, "", map[string]any{"note": "death certificate on file"})
	if st != http.StatusOK {
		t.Fatalf("approve: %d %s", st, body)
	}
	if state := lendingtest.Decode[struct {
		State string `json:"state"`
	}](t, body).State; state != "EXECUTED" {
		t.Fatalf("action state %s", state)
	}

	// Books: principal 10,000,000 + interest 400,000 + late fee 50,000.
	const writtenOff = 10_450_000
	if got := ledgerPosted(t, env, contract.AccountLoanWriteOffExpense); got != writtenOff {
		t.Fatalf("write-off expense %d, want %d", got, writtenOff)
	}
	for _, acct := range []string{contract.AccountLoansPrincipal, contract.AccountLoansInterestReceivable, contract.AccountLoansFeesReceivable} {
		if got := ledgerPosted(t, env, acct); got != 0 {
			t.Fatalf("%s still holds %d after write-off", acct, got)
		}
	}
	login(t, env, &c)
	if l := getLoan(t, env, c, loanID); l.Status != "IN_RECOVERY" {
		t.Fatalf("loan after write-off: %+v", l)
	}
	reconcileClean(t, env)

	// A written-off loan no longer accrues or attracts fees.
	advanceTo(t, env, today(env).AddDate(0, 0, 10))
	dailyJobs(t, env)
	if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != 0 {
		t.Fatalf("interest accrued on a written-off loan: %d", got)
	}

	// Money received later is a recovery, capped at what was written off.
	env.Ledger.Fund(t, c.AccountCode, 2_000_000)
	login(t, env, &c)
	st, body = repay(t, env, c, loanID, 3_000_000)
	rep := lendingtest.Decode[repaymentView](t, body)
	if st != http.StatusCreated || rep.AppliedMinor != 3_000_000 {
		t.Fatalf("recovery: %d %+v", st, rep)
	}
	if got := ledgerPosted(t, env, contract.AccountRecoveriesIncome); got != 3_000_000 {
		t.Fatalf("recoveries income %d", got)
	}
	// Paying more than remains takes only what remains and closes the loan.
	env.Ledger.Fund(t, c.AccountCode, 10_000_000)
	st, body = repay(t, env, c, loanID, 20_000_000)
	rep = lendingtest.Decode[repaymentView](t, body)
	if st != http.StatusCreated || rep.AppliedMinor != writtenOff-3_000_000 || rep.UnappliedMinor != 20_000_000-rep.AppliedMinor {
		t.Fatalf("final recovery: %d %+v", st, rep)
	}
	if l := getLoan(t, env, c, loanID); l.Status != "CLOSED" {
		t.Fatalf("loan after full recovery: %+v", l)
	}
	if got := ledgerPosted(t, env, contract.AccountRecoveriesIncome); got != writtenOff {
		t.Fatalf("recoveries income %d, want %d", got, writtenOff)
	}
	reconcileClean(t, env)

	// The whole sequence is in the audit trail with who did what.
	for _, action := range []string{"ADMIN_ACTION_PROPOSED", "ADMIN_ACTION_APPROVED", "LOAN_WRITTEN_OFF", "RECOVERY_RECEIVED"} {
		if n := count(t, env, `SELECT count(*) FROM lending.audit_log WHERE entity_id = $1 AND action = $2`, loanID, action); n == 0 {
			t.Fatalf("audit trail lacks %s", action)
		}
	}
}

// relogin issues a fresh staff member with the given roles (staff tokens
// expire as the test moves the clock).
func relogin(t *testing.T, env *lendingtest.Env, roles ...string) (string, string) {
	t.Helper()
	id, tok := env.Identity.StaffToken(t, roles...)
	return id.String(), tok
}

func TestRestructureCreatesANewScheduleVersion(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	loan := getLoan(t, env, c, loanID)
	due := firstDue(t, loan)

	// Pay the first instalment, then fall 20 days behind on the second.
	advanceTo(t, env, due)
	login(t, env, &c)
	dailyJobs(t, env)
	if st, body := repay(t, env, c, loanID, 3_603_485); st != http.StatusCreated {
		t.Fatalf("first instalment: %d %s", st, body)
	}
	advanceTo(t, env, bizdate.AddMonths(due, 1).AddDate(0, 0, 20))
	dailyJobs(t, env)

	_, maker := env.Identity.StaffToken(t, authn.RoleOpsMaker)
	_, checker := env.Identity.StaffToken(t, authn.RoleOpsChecker)

	// The new tenor is bounded by the product.
	if st, body := env.Do(t, "POST", "/v1/ops/admin-actions", maker, "", map[string]any{
		"kind": "RESTRUCTURE", "loan_id": loanID, "params": map[string]any{"new_tenor_months": 60}, "reason": "customer lost employment"}); st != 400 {
		t.Fatalf("over-long restructure: %d %s", st, body)
	}
	st, body := env.Do(t, "POST", "/v1/ops/admin-actions", maker, "", map[string]any{
		"kind": "RESTRUCTURE", "loan_id": loanID, "params": map[string]any{"new_tenor_months": 6}, "reason": "customer lost employment; new job from next month"})
	if st != http.StatusCreated {
		t.Fatalf("propose: %d %s", st, body)
	}
	actionID := lendingtest.Decode[struct {
		ActionID string `json:"action_id"`
	}](t, body).ActionID

	// A rejected proposal changes nothing and frees the loan for a new one.
	if st, body := env.Do(t, "POST", "/v1/ops/admin-actions/"+actionID+"/reject", checker, "", map[string]any{"note": "needs evidence of the new job"}); st != http.StatusOK {
		t.Fatalf("reject: %d %s", st, body)
	}
	login(t, env, &c)
	if l := getLoan(t, env, c, loanID); l.ScheduleVersion != 1 || l.Restructured {
		t.Fatalf("loan changed by a rejected action: %+v", l)
	}
	st, body = env.Do(t, "POST", "/v1/ops/admin-actions", maker, "", map[string]any{
		"kind": "RESTRUCTURE", "loan_id": loanID, "params": map[string]any{"new_tenor_months": 6}, "reason": "offer letter received and verified"})
	if st != http.StatusCreated {
		t.Fatalf("second proposal: %d %s", st, body)
	}
	actionID = lendingtest.Decode[struct {
		ActionID string `json:"action_id"`
	}](t, body).ActionID
	if st, body := env.Do(t, "POST", "/v1/ops/admin-actions/"+actionID+"/approve", checker, "", map[string]any{"note": "evidence reviewed"}); st != http.StatusOK {
		t.Fatalf("approve: %d %s", st, body)
	}

	l := getLoan(t, env, c, loanID)
	if l.ScheduleVersion != 2 || !l.Restructured || l.Status != "ACTIVE" || l.DaysPastDue != 0 {
		t.Fatalf("loan after restructure: %+v", l)
	}
	// One paid instalment kept, six new ones.
	if len(l.Instalments) != 7 || !l.Instalments[0].Paid {
		t.Fatalf("restructured schedule: %+v", l.Instalments)
	}
	var principal int64
	for _, i := range l.Instalments {
		principal += i.PrincipalMinor
	}
	if principal != 10_000_000 {
		t.Fatalf("restructured principal sums to %d, want the original 10,000,000", principal)
	}
	// The interest already earned on the overdue instalment is carried onto
	// the first new instalment, not forgiven and not charged twice.
	if l.Instalments[1].InterestMinor <= l.Instalments[2].InterestMinor {
		t.Fatalf("carried interest missing from the first new instalment: %+v", l.Instalments[1:3])
	}
	// A restructure moves no money: the ledger still agrees with the book.
	reconcileClean(t, env)
	// The old schedule and the fact of the arrears are both kept.
	if n := count(t, env, `SELECT count(*) FROM lending.instalments WHERE loan_id = $1 AND schedule_version = 1`, loanID); n != 3 {
		t.Fatalf("old schedule has %d rows", n)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.audit_log WHERE entity_id = $1 AND action = 'ADMIN_ACTION_APPROVED' AND (detail->>'previous_days_past_due')::int = 20`, loanID); n != 1 {
		t.Fatalf("the audit trail does not record the arrears before the restructure")
	}

	// The restructured loan can be repaid to closure.
	env.Ledger.Fund(t, c.AccountCode, 6_000_000)
	st, body = repay(t, env, c, loanID, 20_000_000)
	if st != http.StatusCreated || !lendingtest.Decode[repaymentView](t, body).SettlesLoan {
		t.Fatalf("settling the restructured loan: %d %s", st, body)
	}
	advanceTo(t, env, today(env).AddDate(0, 0, 2))
	dailyJobs(t, env)
	reconcileClean(t, env)
}
