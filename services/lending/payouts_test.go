package lending_test

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/identity/identitytest"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/lendingtest"
	sim "bankplatform.internal/simulator"
)

// externalAccept accepts an offer with an external destination and returns
// the loan. The loan is booked to the deposit account first (T3); the payout
// is then READY for the driver.
func externalAccept(t *testing.T, env *lendingtest.Env, c identitytest.Customer, accountNumber string) (loanView, domain.Payout) {
	t.Helper()
	_, offer := offered(t, env, c, amount100k, 3)
	body := acceptBody(offer, c.PIN)
	body["destination"] = map[string]any{"type": "EXTERNAL_BANK_ACCOUNT", "bank_code": "058", "account_number": accountNumber}
	st, resp := accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), body)
	if st != http.StatusCreated {
		t.Fatalf("accept with external destination: %d %s", st, resp)
	}
	loan := lendingtest.Decode[loanView](t, resp)
	if loan.Status != "ACTIVE" || loan.Payout == nil || loan.Payout.Status != "PROCESSING" {
		t.Fatalf("loan after accept: %+v", loan)
	}
	// Booked to the deposit account before anything is sent anywhere.
	if posted, held := balance(t, env, c); posted != 9_900_000 || held != 0 {
		t.Fatalf("balance after booking: %d held %d", posted, held)
	}
	p, err := env.Store.PayoutByLoan(ctx, uuid.MustParse(loan.LoanID))
	if err != nil {
		t.Fatal(err)
	}
	return loan, p
}

func drive(t *testing.T, env *lendingtest.Env) {
	t.Helper()
	if err := env.Service.DriveDuePayouts(ctx); err != nil {
		t.Logf("drive payouts: %v", err) // transient provider errors are expected in several tests
	}
}

func payoutState(t *testing.T, env *lendingtest.Env, id uuid.UUID) domain.Payout {
	t.Helper()
	p, err := env.Store.GetPayout(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPayoutSuccessIsCapturedAndSettled(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loan, p := externalAccept(t, env, c, "0123456701")
	drive(t, env)

	got := payoutState(t, env, p.ID)
	if got.State != domain.PayoutSucceeded || got.ProviderRef == "" {
		t.Fatalf("payout %+v", got)
	}
	if posted, held := balance(t, env, c); posted != 0 || held != 0 {
		t.Fatalf("after capture: posted %d held %d, want 0/0", posted, held)
	}
	if got := ledgerPosted(t, env, contract.AccountPayoutClearing); got != 9_900_000 {
		t.Fatalf("payout clearing %d: the bank owes the rail this until settlement", got)
	}
	if l := getLoan(t, env, c, loan.LoanID); l.Payout.Status != "SENT" || l.Payout.AccountLast4 != "6701" {
		t.Fatalf("customer view %+v", l.Payout)
	}
	if tr, ok := env.Sim.Transfer(p.Reference); !ok || tr.SendCount != 1 || tr.AmountMinor != 9_900_000 {
		t.Fatalf("provider record %+v", tr)
	}

	// Settlement: the provider's report confirms it; clearing is discharged
	// from cash at the settlement bank.
	res, err := env.Service.ReconcilePayouts(ctx, today(env))
	if err != nil || res.Settled != 1 || res.Exceptions != 0 {
		t.Fatalf("payout reconciliation %+v %v", res, err)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutSettled {
		t.Fatalf("payout after settlement: %s", got.State)
	}
	if got := ledgerPosted(t, env, contract.AccountPayoutClearing); got != 0 {
		t.Fatalf("payout clearing %d after settlement", got)
	}
	if got := ledgerPosted(t, env, contract.AccountCashSettlementBank); got != -9_900_000 {
		t.Fatalf("cash at settlement bank %d, want -9,900,000 (paid out)", got)
	}
	// Reconciling the same date again changes nothing.
	if res, err := env.Service.ReconcilePayouts(ctx, today(env)); err != nil || res.Settled != 0 || res.Exceptions != 0 {
		t.Fatalf("second reconciliation %+v %v", res, err)
	}
	if n := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_PAYOUT_SETTLEMENT'`); n != 1 {
		t.Fatalf("%d settlement journals", n)
	}
	reconcileClean(t, env)
}

func TestPayoutRejectionReleasesTheFunds(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loan, p := externalAccept(t, env, c, "0123456790") // provider rejects
	drive(t, env)

	got := payoutState(t, env, p.ID)
	if got.State != domain.PayoutFailed || got.LastCode != "ACCOUNT_CLOSED" {
		t.Fatalf("payout %+v", got)
	}
	// The proceeds are back in the customer's available balance; the loan stands.
	if posted, held := balance(t, env, c); posted != 9_900_000 || held != 0 {
		t.Fatalf("after rejection: posted %d held %d", posted, held)
	}
	if l := getLoan(t, env, c, loan.LoanID); l.Status != "ACTIVE" || l.Payout.Status != "FAILED" {
		t.Fatalf("loan %+v payout %+v", l, l.Payout)
	}
	if got := ledgerPosted(t, env, contract.AccountPayoutClearing); got != 0 {
		t.Fatalf("payout clearing %d after a rejection", got)
	}
	reconcileClean(t, env)
}

// Provider success with a lost response: the transfer went through but our
// call timed out. The funds stay held, the customer sees "processing", the
// transfer is NOT sent again, and a status query resolves it.
func TestPayoutTimeoutIsUnknownNotFailed(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loan, p := externalAccept(t, env, c, "0123456791") // success, response never arrives
	drive(t, env)

	got := payoutState(t, env, p.ID)
	if got.State != domain.PayoutUnknown {
		t.Fatalf("after a timeout the payout is %s, want UNKNOWN", got.State)
	}
	// Neither refunded nor spent: held.
	if posted, held := balance(t, env, c); posted != 9_900_000 || held != 9_900_000 {
		t.Fatalf("during the unknown period: posted %d held %d, want the amount held", posted, held)
	}
	if l := getLoan(t, env, c, loan.LoanID); l.Payout.Status != "PROCESSING" {
		t.Fatalf("customer sees %s, want PROCESSING", l.Payout.Status)
	}
	// The customer cannot spend the held money meanwhile.
	if st, body := repay(t, env, c, loan.LoanID, 100_000); st != http.StatusUnprocessableEntity && st != http.StatusConflict {
		t.Fatalf("spending held funds: %d %s", st, body)
	}

	// Not due yet: driving again must not query early, and must never resend.
	drive(t, env)
	// After the backoff, the status query finds the success.
	env.Clock.Advance(env.Config.PayoutQueryBackoff[0] + time.Second)
	drive(t, env)

	if got := payoutState(t, env, p.ID); got.State != domain.PayoutSucceeded {
		t.Fatalf("after the status query: %s", got.State)
	}
	if posted, held := balance(t, env, c); posted != 0 || held != 0 {
		t.Fatalf("after capture: posted %d held %d", posted, held)
	}
	tr, _ := env.Sim.Transfer(p.Reference)
	if tr.SendCount != 1 || env.Sim.TransferCount() != 1 {
		t.Fatalf("the transfer was sent %d times (%d transfers at the provider); it must be sent exactly once", tr.SendCount, env.Sim.TransferCount())
	}
	reconcileClean(t, env)
}

// Timeout where the transfer in fact failed: resolved to FAILED by the
// status query, and only then are the funds released.
func TestPayoutTimeoutResolvedAsFailureReleasesOnlyThen(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, p := externalAccept(t, env, c, "0123456792")
	drive(t, env)
	if _, held := balance(t, env, c); held != 9_900_000 {
		t.Fatalf("held %d while unknown", held)
	}
	env.Clock.Advance(env.Config.PayoutQueryBackoff[0] + time.Second)
	drive(t, env)
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutFailed {
		t.Fatalf("payout %s", got.State)
	}
	if posted, held := balance(t, env, c); posted != 9_900_000 || held != 0 {
		t.Fatalf("after confirmed failure: posted %d held %d", posted, held)
	}
}

// A malformed response and a provider-side PENDING are both unknown
// outcomes; a delayed success is found by later queries.
func TestPayoutMalformedAndDelayedResponses(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	for _, account := range []string{"0123456794", "0123456793"} { // malformed; pending then success
		c := env.Identity.Register(t)
		_, p := externalAccept(t, env, c, account)
		drive(t, env)
		if got := payoutState(t, env, p.ID); got.State != domain.PayoutUnknown {
			t.Fatalf("%s: after send %s, want UNKNOWN", account, got.State)
		}
		for i := 0; i < 4 && payoutState(t, env, p.ID).State == domain.PayoutUnknown; i++ {
			env.Clock.Advance(env.Config.PayoutQueryBackoff[min(i, len(env.Config.PayoutQueryBackoff)-1)] + time.Second)
			drive(t, env)
		}
		if got := payoutState(t, env, p.ID); got.State != domain.PayoutSucceeded {
			t.Fatalf("%s: final state %s", account, got.State)
		}
		if tr, _ := env.Sim.Transfer(p.Reference); tr.SendCount != 1 {
			t.Fatalf("%s: sent %d times", account, tr.SendCount)
		}
	}
	reconcileClean(t, env)
}

// The worker dies after recording SENDING and before learning the result.
// On recovery the payout is treated as unknown and resolved by query. It is
// not sent a second time.
func TestPayoutSenderCrashIsResolvedByQueryNotResend(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, p := externalAccept(t, env, c, "0123456701")

	// Reproduce the crash point exactly: hold placed, SENDING recorded, the
	// request reached the provider and succeeded, and then the process died.
	if err := env.Faults.PlaceHoldDirect(ctx, contract.HoldLoanPayout, p.ID, c.AccountCode, p.AmountMinor); err != nil {
		t.Fatal(err)
	}
	now := env.Clock.Now()
	if _, err := env.Pool.Exec(ctx, `UPDATE lending.payouts SET state = 'SENDING', next_attempt_at = $2, state_changed_at = $2 WHERE payout_id = $1`, p.ID, now); err != nil {
		t.Fatal(err)
	}
	env.Sim.SeedTransfer(p.Reference, p.AmountMinor, sim.StatusSuccess)

	env.Clock.Advance(time.Minute)
	drive(t, env)

	if got := payoutState(t, env, p.ID); got.State != domain.PayoutSucceeded {
		t.Fatalf("after recovery: %s", got.State)
	}
	if tr, _ := env.Sim.Transfer(p.Reference); tr.SendCount != 1 {
		t.Fatalf("recovery re-sent the transfer (%d sends)", tr.SendCount)
	}
	if posted, held := balance(t, env, c); posted != 0 || held != 0 {
		t.Fatalf("after recovery: posted %d held %d", posted, held)
	}
	reconcileClean(t, env)
}

// The provider never recorded the transfer (it failed before processing).
// A status query answers NOT_FOUND, which is not proof of failure, so the
// funds stay held. Only a settlement report for the closed day, from a
// provider whose contract makes that report final, releases them.
func TestPayoutNotFoundStaysUnknownUntilTheSettlementReport(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{Tune: func(c *app.Config) { c.AbsentFromReportMeansFailed = true }})
	c := env.Identity.Register(t)
	_, p := externalAccept(t, env, c, "0123456795") // provider returns 500 and records nothing
	sentOn := today(env)
	drive(t, env)

	for i := range 3 {
		env.Clock.Advance(env.Config.PayoutQueryBackoff[i] + time.Second)
		drive(t, env)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutUnknown {
		t.Fatalf("after NOT_FOUND queries: %s, want UNKNOWN", got.State)
	}
	if _, held := balance(t, env, c); held != 9_900_000 {
		t.Fatalf("held %d: funds must stay held while the outcome is unknown", held)
	}

	// Same day: the report is not final yet, so absence proves nothing.
	if _, err := env.Service.ReconcilePayouts(ctx, sentOn); err != nil {
		t.Fatal(err)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutUnknown {
		t.Fatalf("released on a same-day report: %s", got.State)
	}

	// After 30 minutes unresolved, operations are alerted.
	env.Clock.Advance(31 * time.Minute)
	drive(t, env)
	open, _ := env.Service.ReconExceptions(ctx, "OPEN")
	if len(open) != 1 || open[0].Kind != domain.ExceptionUnresolved {
		t.Fatalf("exceptions %+v", open)
	}

	// Next day: the closed day's report does not contain the transfer.
	advanceTo(t, env, sentOn.AddDate(0, 0, 1))
	res, err := env.Service.ReconcilePayouts(ctx, sentOn)
	if err != nil || res.Resolved != 1 {
		t.Fatalf("reconciliation %+v %v", res, err)
	}
	got := payoutState(t, env, p.ID)
	if got.State != domain.PayoutFailed || got.LastCode != "ABSENT_FROM_SETTLEMENT_REPORT" {
		t.Fatalf("after the final report: %+v", got)
	}
	if posted, held := balance(t, env, c); posted != 9_900_000 || held != 0 {
		t.Fatalf("after release: posted %d held %d", posted, held)
	}
}

// Without a contractual guarantee that the report is final, absence from it
// never releases funds.
func TestAbsenceFromReportDoesNotReleaseByDefault(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, p := externalAccept(t, env, c, "0123456795")
	sentOn := today(env)
	drive(t, env)
	advanceTo(t, env, sentOn.AddDate(0, 0, 1))
	if _, err := env.Service.ReconcilePayouts(ctx, sentOn); err != nil {
		t.Fatal(err)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutUnknown {
		t.Fatalf("payout %s, want still UNKNOWN", got.State)
	}
	if _, held := balance(t, env, c); held != 9_900_000 {
		t.Fatalf("held %d", held)
	}
}

func TestPayoutCallbacks(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})

	t.Run("a callback resolves an unknown payout; duplicates are harmless", func(t *testing.T) {
		c := env.Identity.Register(t)
		_, p := externalAccept(t, env, c, "0123456796") // pending until resolved
		drive(t, env)
		if got := payoutState(t, env, p.ID); got.State != domain.PayoutUnknown {
			t.Fatalf("payout %s", got.State)
		}
		env.Sim.Resolve(p.Reference, sim.StatusSuccess, "00")

		event := uuid.NewString()
		for range 5 { // the provider delivers the same event five times
			if err := env.Sim.SendCallback(ctx, event, p.Reference, ""); err != nil {
				t.Fatalf("callback: %v", err)
			}
		}
		if got := payoutState(t, env, p.ID); got.State != domain.PayoutSucceeded {
			t.Fatalf("after callback: %s", got.State)
		}
		if posted, held := balance(t, env, c); posted != 0 || held != 0 {
			t.Fatalf("after callback: posted %d held %d (a duplicate must not debit twice)", posted, held)
		}
		if n := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_PAYOUT_CAPTURE' AND op_id = $1`, p.ID); n != 1 {
			t.Fatalf("%d capture journals", n)
		}
		if n := count(t, env, `SELECT count(*) FROM lending.payout_events WHERE reference = $1`, p.Reference); n != 1 {
			t.Fatalf("%d callback events recorded, want 1", n)
		}
		if applied, dup := env.Metrics.Count("callback:applied"), env.Metrics.Count("callback:duplicate"); applied != 1 || dup != 4 {
			t.Fatalf("callbacks counted applied=%d duplicate=%d, want 1/4", applied, dup)
		}
		// A different event id carrying the same news is also harmless.
		if err := env.Sim.SendCallback(ctx, uuid.NewString(), p.Reference, ""); err != nil {
			t.Fatal(err)
		}
		if posted, _ := balance(t, env, c); posted != 0 {
			t.Fatalf("balance %d after a repeated success", posted)
		}
	})

	t.Run("forged and stale callbacks are rejected and change nothing", func(t *testing.T) {
		c := env.Identity.Register(t)
		_, p := externalAccept(t, env, c, "0123456796")
		drive(t, env)

		body := []byte(fmt.Sprintf(`{"event_id":"%s","reference":"%s","status":"SUCCESS","provider_ref":"PX-1","code":"00"}`, uuid.NewString(), p.Reference))
		ts := strconv.FormatInt(env.Clock.Now().Unix(), 10)

		// Wrong signature.
		if err := env.Sim.PostCallback(ctx, body, ts, "deadbeef"); err == nil {
			t.Fatal("a forged callback was accepted")
		}
		// Signed with another secret.
		if err := env.Sim.PostCallback(ctx, body, ts, sim.Sign("not-the-secret", ts, body)); err == nil {
			t.Fatal("a callback signed with the wrong secret was accepted")
		}
		// Correctly signed but an hour old: a replay.
		old := strconv.FormatInt(env.Clock.Now().Add(-time.Hour).Unix(), 10)
		if err := env.Sim.PostCallback(ctx, body, old, ""); err == nil {
			t.Fatal("a stale callback was accepted")
		}
		// Signature valid for a different body (tampered amount of text).
		tampered := []byte(fmt.Sprintf(`{"event_id":"%s","reference":"%s","status":"FAILED","provider_ref":"PX-1","code":"00"}`, uuid.NewString(), p.Reference))
		if err := env.Sim.PostCallback(ctx, tampered, ts, sim.Sign("sim-callback-secret", ts, body)); err == nil {
			t.Fatal("a tampered callback was accepted")
		}

		if got := payoutState(t, env, p.ID); got.State != domain.PayoutUnknown {
			t.Fatalf("a rejected callback changed the payout to %s", got.State)
		}
		if _, held := balance(t, env, c); held != 9_900_000 {
			t.Fatalf("held %d after rejected callbacks", held)
		}
		if n := env.Metrics.Count("callback:rejected"); n != 4 {
			t.Fatalf("%d rejected callbacks counted, want 4", n)
		}
	})

	t.Run("a callback for an unknown reference raises an exception", func(t *testing.T) {
		env.Sim.SeedTransfer("LP-not-ours", 123_456, sim.StatusSuccess)
		if err := env.Sim.SendCallback(ctx, uuid.NewString(), "LP-not-ours", ""); err != nil {
			t.Fatal(err)
		}
		open, _ := env.Service.ReconExceptions(ctx, "OPEN")
		found := false
		for _, e := range open {
			if e.Kind == domain.ExceptionUnknownAtUs && e.EntityID == "LP-not-ours" {
				found = true
			}
		}
		if !found {
			t.Fatalf("no exception for the unknown reference: %+v", open)
		}
	})
}

// Out-of-order news: the provider first says FAILED (we release the funds),
// then proves SUCCESS. The transfer happened, so the customer is debited by
// a late-capture journal and the records end consistent.
func TestLateSuccessAfterRecordedFailureIsDebited(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, p := externalAccept(t, env, c, "0123456796")
	drive(t, env)

	if err := env.Sim.SendCallback(ctx, uuid.NewString(), p.Reference, sim.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutFailed {
		t.Fatalf("after FAILED callback: %s", got.State)
	}
	if posted, held := balance(t, env, c); posted != 9_900_000 || held != 0 {
		t.Fatalf("after release: posted %d held %d", posted, held)
	}

	// Then the truth arrives.
	env.Sim.Resolve(p.Reference, sim.StatusSuccess, "00")
	if err := env.Sim.SendCallback(ctx, uuid.NewString(), p.Reference, ""); err != nil {
		t.Fatal(err)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutSucceeded || got.LastCode != "LATE_SUCCESS" {
		t.Fatalf("after late success: %+v", got)
	}
	if posted, held := balance(t, env, c); posted != 0 || held != 0 {
		t.Fatalf("after late capture: posted %d held %d", posted, held)
	}
	if n := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_PAYOUT_LATE_CAPTURE'`); n != 1 {
		t.Fatalf("%d late-capture journals", n)
	}
	// The exception was raised and then closed by the system once the books agreed.
	if open, _ := env.Service.ReconExceptions(ctx, "OPEN"); len(open) != 0 {
		t.Fatalf("open exceptions %+v", open)
	}
	resolved, _ := env.Service.ReconExceptions(ctx, "RESOLVED")
	if len(resolved) != 1 || resolved[0].Kind != domain.ExceptionLateSuccess {
		t.Fatalf("resolved exceptions %+v", resolved)
	}
	reconcileClean(t, env)
}

// The same late success when the customer has already spent the released
// money: the debit is refused, the exception stays open for operations, and
// the payout is NOT quietly marked successful.
func TestLateSuccessWithoutFundsStaysAnOpenException(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loan, p := externalAccept(t, env, c, "0123456796")
	drive(t, env)
	if err := env.Sim.SendCallback(ctx, uuid.NewString(), p.Reference, sim.StatusFailed); err != nil {
		t.Fatal(err)
	}
	// The customer uses the released funds to settle the loan early.
	env.Ledger.Fund(t, c.AccountCode, 500_000)
	advanceTo(t, env, today(env).AddDate(0, 0, 1))
	login(t, env, &c)
	if st, body := repay(t, env, c, loan.LoanID, 10_400_000); st != http.StatusCreated {
		t.Fatalf("settle: %d %s", st, body)
	}

	env.Sim.Resolve(p.Reference, sim.StatusSuccess, "00")
	if err := env.Sim.SendCallback(ctx, uuid.NewString(), p.Reference, ""); err != nil {
		t.Fatal(err)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutFailed {
		t.Fatalf("payout %s: it must not be marked successful while the books disagree", got.State)
	}
	open, _ := env.Service.ReconExceptions(ctx, "OPEN")
	if len(open) != 1 || open[0].Kind != domain.ExceptionLateSuccess || open[0].AmountMinor != 9_900_000 {
		t.Fatalf("exceptions %+v", open)
	}
	if posted, _ := balance(t, env, c); posted < 0 {
		t.Fatalf("customer balance went negative: %d", posted)
	}
	env.Ledger.AssertInvariants(t)
}

// Settlement reports that disagree with our records raise exceptions.
func TestSettlementReportDisagreementsRaiseExceptions(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, p := externalAccept(t, env, c, "0123456701")
	drive(t, env)
	day := today(env)

	// 1. The report omits a transfer we captured as successful.
	env.Sim.HideFromReport(p.Reference)
	// 2. The report contains a transfer we have never heard of.
	env.Sim.AddReportItem("LP-stranger", "PX-9", 777_000, sim.StatusSuccess)

	res, err := env.Service.ReconcilePayouts(ctx, day)
	if err != nil || res.Exceptions != 2 || res.Settled != 0 {
		t.Fatalf("reconciliation %+v %v", res, err)
	}
	kinds := map[string]bool{}
	open, _ := env.Service.ReconExceptions(ctx, "OPEN")
	for _, e := range open {
		kinds[e.Kind] = true
	}
	if !kinds[domain.ExceptionMissingAtProvider] || !kinds[domain.ExceptionUnknownAtUs] || len(open) != 2 {
		t.Fatalf("exceptions %+v", open)
	}
	// Nothing was settled on the strength of a disputed report.
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutSucceeded {
		t.Fatalf("payout %s", got.State)
	}
	if got := ledgerPosted(t, env, contract.AccountCashSettlementBank); got != 0 {
		t.Fatalf("cash moved on a disputed report: %d", got)
	}

	// Operations resolve an exception with a note; a viewer cannot.
	_, viewer := env.Identity.StaffToken(t, "ops_viewer")
	_, checker := env.Identity.StaffToken(t, "ops_checker")
	if st, _ := env.Do(t, "POST", "/v1/ops/recon-exceptions/"+open[0].ID.String()+"/resolve", viewer, "", map[string]any{"note": "confirmed with provider by email"}); st != http.StatusForbidden {
		t.Fatalf("viewer resolving: %d", st)
	}
	if st, body := env.Do(t, "POST", "/v1/ops/recon-exceptions/"+open[0].ID.String()+"/resolve", checker, "", map[string]any{"note": "confirmed with provider by email; ticket 4471"}); st != http.StatusNoContent {
		t.Fatalf("resolve: %d %s", st, body)
	}
	if open, _ := env.Service.ReconExceptions(ctx, "OPEN"); len(open) != 1 {
		t.Fatalf("%d open after resolving one", len(open))
	}
}

func TestExternalDestinationMustBeTheCustomersOwnAccount(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	applicationID, offer := offered(t, env, c, amount100k, 3)

	try := func(account string) (int, []byte) {
		body := acceptBody(offer, c.PIN)
		body["destination"] = map[string]any{"type": "EXTERNAL_BANK_ACCOUNT", "bank_code": "058", "account_number": account}
		return accept(t, env, c.Token, offer.OfferID, lendingtest.Key(), body)
	}
	// The account belongs to someone else.
	if st, body := try("0123456799"); st != 422 || lendingtest.ErrorCode(t, body) != "DESTINATION_NOT_VERIFIED" {
		t.Fatalf("someone else's account: %d %s", st, body)
	}
	// The account does not exist.
	if st, body := try("0123456798"); st != 422 || lendingtest.ErrorCode(t, body) != "DESTINATION_NOT_VERIFIED" {
		t.Fatalf("unknown account: %d %s", st, body)
	}
	// Name enquiry is unavailable: not a rejection, a retryable 503.
	if st, body := try("0123456797"); st != 503 || lendingtest.ErrorCode(t, body) != "DEPENDENCY_UNAVAILABLE" {
		t.Fatalf("enquiry timeout: %d %s", st, body)
	}
	// Malformed account number.
	if st, body := try("12345"); st != 400 {
		t.Fatalf("malformed account: %d %s", st, body)
	}
	// None of that consumed the offer.
	if a := getApplication(t, env, c, applicationID); a.Status != "OFFER_READY" {
		t.Fatalf("offer consumed by refused destinations: %+v", a)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.payouts`); n != 0 {
		t.Fatalf("%d payouts created", n)
	}
	// A name in a different order and case still matches.
	env.Sim.SetAccountName("0123456701", "CUSTOMER, Ada Test")
	if st, body := try("0123456701"); st != http.StatusCreated {
		t.Fatalf("own account: %d %s", st, body)
	}
}

func TestNamesMatch(t *testing.T) {
	cases := []struct {
		verified, enquired string
		want               bool
	}{
		{"Ada Test Customer", "ADA TEST CUSTOMER", true},
		{"Ada Test Customer", "Customer, Ada Test", true},
		{"Ada Test Customer", "ADA CUSTOMER", true},
		{"Ada Test Customer", "Ada Okafor", false},
		{"Ada Test Customer", "Ada", false},
		{"Ada Test Customer", "SOMEONE ELSE ENTIRELY", false},
		{"Ada Test Customer", "", false},
		{"Chukwuemeka O'Brien-Okoro", "OKORO CHUKWUEMEKA O BRIEN", true},
	}
	for _, c := range cases {
		if got := app.NamesMatch(c.verified, c.enquired); got != c.want {
			t.Errorf("NamesMatch(%q, %q) = %v, want %v", c.verified, c.enquired, got, c.want)
		}
	}
}

// If the customer has already moved the proceeds by the time the payout is
// attempted, nothing is held and nothing is sent.
func TestPayoutFailsCleanlyWhenFundsAreGone(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loan, p := externalAccept(t, env, c, "0123456701")
	// Settle the loan from the proceeds before the driver runs.
	env.Ledger.Fund(t, c.AccountCode, 200_000)
	if st, body := repay(t, env, c, loan.LoanID, 10_100_000); st != http.StatusCreated {
		t.Fatalf("settle: %d %s", st, body)
	}
	drive(t, env)
	got := payoutState(t, env, p.ID)
	if got.State != domain.PayoutFailed || got.LastCode != "INSUFFICIENT_FUNDS" {
		t.Fatalf("payout %+v", got)
	}
	if env.Sim.TransferCount() != 0 {
		t.Fatal("a transfer was sent without the funds being held first")
	}
}
