package lending_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/lendingtest"
)

// firstDue returns the loan's first due date.
func firstDue(t testing.TB, l loanView) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, l.Instalments[0].DueDate)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPartialRepaymentPaysInterestThenPrincipal(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	loan := getLoan(t, env, c, loanID)
	inst := loan.Instalments[0] // interest 400,000; principal 3,203,485

	advanceTo(t, env, firstDue(t, loan))
	login(t, env, &c)
	dailyJobs(t, env)

	// Less than the interest due: all of it is interest.
	st, body := repay(t, env, c, loanID, 150_000)
	rep := lendingtest.Decode[repaymentView](t, body)
	if st != http.StatusCreated || rep.InterestMinor != 150_000 || rep.PrincipalMinor != 0 {
		t.Fatalf("first partial: %d %+v", st, rep)
	}
	// The rest of the interest, and some principal.
	st, body = repay(t, env, c, loanID, 1_000_000)
	rep = lendingtest.Decode[repaymentView](t, body)
	if st != http.StatusCreated || rep.InterestMinor != inst.InterestMinor-150_000 || rep.PrincipalMinor != 1_000_000-(inst.InterestMinor-150_000) {
		t.Fatalf("second partial: %d %+v", st, rep)
	}
	got := getLoan(t, env, c, loanID).Instalments[0]
	if got.Paid || got.InterestPaid != inst.InterestMinor || got.PrincipalPaid != 750_000 {
		t.Fatalf("instalment after partials: %+v", got)
	}
	reconcileClean(t, env)
}

// An overpayment never leaves the customer's account: only what the product
// rules can apply is debited.
func TestOverpaymentIsNotTaken(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	loan := getLoan(t, env, c, loanID)
	inst := loan.Instalments[0]

	// Ten days in, the customer offers 5,000,000: more than the next
	// instalment, less than the payoff. Pay-ahead takes the interest earned
	// so far plus the next instalment's principal and nothing else.
	advanceTo(t, env, today(env).AddDate(0, 0, 10))
	login(t, env, &c)
	before, _ := balance(t, env, c)

	st, body := repay(t, env, c, loanID, 5_000_000)
	rep := lendingtest.Decode[repaymentView](t, body)
	periodDays := int64(bizdate.Days(today(env).AddDate(0, 0, -10), firstDue(t, loan)))
	wantInterest := inst.InterestMinor * 10 / periodDays
	if st != http.StatusCreated || rep.InterestMinor != wantInterest || rep.PrincipalMinor != inst.PrincipalMinor ||
		rep.AppliedMinor != wantInterest+inst.PrincipalMinor || rep.UnappliedMinor != 5_000_000-rep.AppliedMinor || rep.SettlesLoan {
		t.Fatalf("pay-ahead: %d %+v (want interest %d)", st, rep, wantInterest)
	}
	after, _ := balance(t, env, c)
	if before-after != rep.AppliedMinor {
		t.Fatalf("debited %d, want exactly the applied amount %d", before-after, rep.AppliedMinor)
	}
	// Paying again the same day: nothing more is payable ahead.
	if st, body := repay(t, env, c, loanID, 1_000_000); st != http.StatusConflict || lendingtest.ErrorCode(t, body) != "LOAN_NOT_REPAYABLE" {
		t.Fatalf("second pay-ahead: %d %s", st, body)
	}
	reconcileClean(t, env)
}

// Settling early costs principal plus the interest earned to date. Interest
// not yet earned is waived, and the loan closes.
func TestEarlySettlementWaivesUnearnedInterestAndCloses(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	env.Ledger.Fund(t, c.AccountCode, 1_000_000)

	advanceTo(t, env, today(env).AddDate(0, 0, 10))
	login(t, env, &c)
	dailyJobs(t, env) // accrual has run through yesterday

	st, body := env.Do(t, "GET", "/v1/loans/"+loanID+"/payoff-quote", c.Token, "", nil)
	if st != http.StatusOK {
		t.Fatalf("payoff quote: %d %s", st, body)
	}
	quote := lendingtest.Decode[payoffView](t, body)
	if quote.PrincipalMinor != 10_000_000 || quote.InterestMinor <= 0 || quote.InterestMinor >= 400_000 || quote.TotalMinor != quote.PrincipalMinor+quote.InterestMinor {
		t.Fatalf("payoff quote %+v", quote)
	}

	before, _ := balance(t, env, c)
	st, body = repay(t, env, c, loanID, before) // offer everything; only the payoff is taken
	rep := lendingtest.Decode[repaymentView](t, body)
	if st != http.StatusCreated || !rep.SettlesLoan || rep.AppliedMinor != quote.TotalMinor || rep.UnappliedMinor != before-quote.TotalMinor {
		t.Fatalf("settlement: %d %+v quote %+v", st, rep, quote)
	}
	after, _ := balance(t, env, c)
	if before-after != quote.TotalMinor {
		t.Fatalf("debited %d, want the payoff %d", before-after, quote.TotalMinor)
	}

	loan := getLoan(t, env, c, loanID)
	if loan.Status != "CLOSED" || loan.ScheduleVersion != 2 {
		t.Fatalf("loan after settlement: status %s schedule version %d", loan.Status, loan.ScheduleVersion)
	}
	var interestCharged int64
	for _, i := range loan.Instalments {
		if !i.Paid {
			t.Fatalf("instalment %d unpaid after settlement", i.Seq)
		}
		interestCharged += i.InterestMinor
	}
	if interestCharged != quote.InterestMinor {
		t.Fatalf("schedule now charges %d interest, want only the %d earned", interestCharged, quote.InterestMinor)
	}
	// The original schedule is kept as version 1.
	if n := count(t, env, `SELECT count(*) FROM lending.instalments WHERE loan_id = $1 AND schedule_version = 1`, loanID); n != 3 {
		t.Fatalf("original schedule version has %d rows", n)
	}

	// Books: no receivables remain; income is exactly the interest earned;
	// months later nothing further has accrued.
	if got := ledgerPosted(t, env, contract.AccountLoansPrincipal); got != 0 {
		t.Fatalf("principal receivable %d", got)
	}
	advanceTo(t, env, today(env).AddDate(0, 3, 0))
	dailyJobs(t, env)
	if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != 0 {
		t.Fatalf("interest receivable %d after settlement", got)
	}
	if got := ledgerPosted(t, env, contract.AccountInterestIncome); got != quote.InterestMinor {
		t.Fatalf("interest income %d, want %d", got, quote.InterestMinor)
	}
	reconcileClean(t, env)

	// A closed loan takes no more payments.
	login(t, env, &c)
	if st, body := repay(t, env, c, loanID, 100_000); st != http.StatusConflict {
		t.Fatalf("repay closed loan: %d %s", st, body)
	}
}

// Early settlement on a day the accrual job has NOT yet run: the interest
// paid exceeds what the ledger has recognised. The job must catch that up
// later so income and receivable still agree.
func TestSettlementBeforeTheAccrualJobIsCaughtUp(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	env.Ledger.Fund(t, c.AccountCode, 1_000_000)

	advanceTo(t, env, today(env).AddDate(0, 0, 7))
	login(t, env, &c)
	// No daily jobs: nothing has been recognised in the ledger yet.
	st, body := repay(t, env, c, loanID, 11_000_000)
	rep := lendingtest.Decode[repaymentView](t, body)
	if st != http.StatusCreated || !rep.SettlesLoan {
		t.Fatalf("settlement: %d %+v", st, rep)
	}
	// Transiently, interest was collected that has not been recognised.
	if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != -rep.InterestMinor {
		t.Fatalf("interest receivable %d, want %d until the accrual job runs", got, -rep.InterestMinor)
	}

	advanceTo(t, env, today(env).AddDate(0, 0, 1))
	dailyJobs(t, env)
	if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != 0 {
		t.Fatalf("interest receivable %d after catch-up", got)
	}
	if got := ledgerPosted(t, env, contract.AccountInterestIncome); got != rep.InterestMinor {
		t.Fatalf("interest income %d, want %d", got, rep.InterestMinor)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.loans WHERE accrual_catchup`); n != 0 {
		t.Fatalf("catch-up flag still set on %d loans", n)
	}
	reconcileClean(t, env)
}

func TestRepaymentWithInsufficientFundsChangesNothing(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	loan := getLoan(t, env, c, loanID)

	// The customer spends the proceeds elsewhere: simulate by moving time to
	// the due date with an empty account (fund nothing, and drain what is there).
	advanceTo(t, env, firstDue(t, loan))
	login(t, env, &c)
	dailyJobs(t, env)
	st, body := repay(t, env, c, loanID, 3_603_485)
	if st != http.StatusCreated {
		t.Fatalf("first instalment: %d %s", st, body)
	}
	left, _ := balance(t, env, c) // 9,900,000 - 3,603,485
	// Ask for more than is left for the second instalment later.
	advanceTo(t, env, bizdate.AddMonths(firstDue(t, loan), 2))
	login(t, env, &c)
	dailyJobs(t, env)

	before := getLoan(t, env, c, loanID)
	key := lendingtest.Key()
	st, body = env.Do(t, "POST", "/v1/loans/"+loanID+"/repayments", c.Token, key, map[string]any{"amount_minor": left + 1, "currency": "NGN"})
	if st != http.StatusUnprocessableEntity || lendingtest.ErrorCode(t, body) != "INSUFFICIENT_FUNDS" {
		t.Fatalf("got %d %s", st, body)
	}
	after := getLoan(t, env, c, loanID)
	for i := range before.Instalments {
		if before.Instalments[i] != after.Instalments[i] {
			t.Fatalf("schedule changed after a refused repayment: %+v -> %+v", before.Instalments[i], after.Instalments[i])
		}
	}
	if posted, _ := balance(t, env, c); posted != left {
		t.Fatalf("balance changed from %d to %d", left, posted)
	}
	// The same key gives the same answer; it does not try the debit again.
	st, body = env.Do(t, "POST", "/v1/loans/"+loanID+"/repayments", c.Token, key, map[string]any{"amount_minor": left + 1, "currency": "NGN"})
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("replay of refused repayment: %d %s", st, body)
	}
	// The refusal did not leave the loan locked: a payment that fits works.
	if st, body := repay(t, env, c, loanID, left); st != http.StatusCreated {
		t.Fatalf("affordable repayment after a refusal: %d %s", st, body)
	}
	reconcileClean(t, env)
}

// Many repayments at once on one loan. Each is either applied in full or
// refused as "operation in progress"; the customer is debited exactly what
// was applied; no instalment is overpaid.
func TestConcurrentRepaymentsNeverOverAllocate(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	env.Ledger.Fund(t, c.AccountCode, 5_000_000)
	loan := getLoan(t, env, c, loanID)

	// Everything is due.
	advanceTo(t, env, bizdate.AddMonths(firstDue(t, loan), 2))
	login(t, env, &c)
	dailyJobs(t, env)
	before, _ := balance(t, env, c)

	var applied atomic.Int64
	var ok, busy atomic.Int64
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body := repay(t, env, c, loanID, 1_000_000)
			switch {
			case st == http.StatusCreated:
				ok.Add(1)
				applied.Add(lendingtest.Decode[repaymentView](t, body).AppliedMinor)
			case st == http.StatusConflict:
				busy.Add(1) // OPERATION_IN_PROGRESS, or nothing left to pay
			default:
				t.Errorf("unexpected %d %s", st, body)
			}
		}()
	}
	wg.Wait()
	t.Logf("%d applied, %d refused as busy", ok.Load(), busy.Load())

	after, _ := balance(t, env, c)
	if before-after != applied.Load() {
		t.Fatalf("customer debited %d but repayments applied %d", before-after, applied.Load())
	}
	var paid int64
	for _, i := range getLoan(t, env, c, loanID).Instalments {
		if i.PrincipalPaid > i.PrincipalMinor || i.InterestPaid > i.InterestMinor {
			t.Fatalf("instalment %d overpaid: %+v", i.Seq, i)
		}
		paid += i.PrincipalPaid + i.InterestPaid + i.FeesPaid
	}
	if paid != applied.Load() {
		t.Fatalf("schedule shows %d paid, repayments applied %d", paid, applied.Load())
	}
	if n := count(t, env, `SELECT count(*) FROM lending.posting_intents WHERE state = 'PENDING'`); n != 0 {
		t.Fatalf("%d intents left pending", n)
	}
	reconcileClean(t, env)
}

// The ledger debits the customer but lending never hears. Recovery applies
// the repayment once: not zero times (customer debited, loan not credited)
// and not twice.
func TestRepaymentIsAppliedOnceAfterALostLedgerResponse(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	loanID := disbursed(t, env, c, amount100k, 3, false)
	loan := getLoan(t, env, c, loanID)
	advanceTo(t, env, firstDue(t, loan))
	login(t, env, &c)
	dailyJobs(t, env)

	key := lendingtest.Key()
	env.Faults.FailAfter("Post", 1)
	st, body := env.Do(t, "POST", "/v1/loans/"+loanID+"/repayments", c.Token, key, map[string]any{"amount_minor": 3_603_485, "currency": "NGN"})
	if st != http.StatusAccepted {
		t.Fatalf("repayment with lost ledger response: %d %s", st, body)
	}
	if rep := lendingtest.Decode[repaymentView](t, body); rep.Status != "PROCESSING" {
		t.Fatalf("status %s, want PROCESSING (never FAILED) while the outcome is unknown", rep.Status)
	}
	// The debit happened in the ledger; the schedule has not been updated.
	if posted, _ := balance(t, env, c); posted != 9_900_000-3_603_485 {
		t.Fatalf("ledger balance %d", posted)
	}
	if getLoan(t, env, c, loanID).Instalments[0].PrincipalPaid != 0 {
		t.Fatal("schedule updated before lending confirmed the posting")
	}
	// While it is unresolved, another repayment is refused, not double-applied.
	if st, body := repay(t, env, c, loanID, 100_000); st != http.StatusConflict || lendingtest.ErrorCode(t, body) != "OPERATION_IN_PROGRESS" {
		t.Fatalf("second repayment while one is pending: %d %s", st, body)
	}

	env.Clock.Advance(time.Minute)
	for range 3 {
		if err := env.Service.ProcessDueIntents(ctx); err != nil {
			t.Fatal(err)
		}
	}
	login(t, env, &c)
	if !getLoan(t, env, c, loanID).Instalments[0].Paid {
		t.Fatal("instalment not paid after recovery")
	}
	if posted, _ := balance(t, env, c); posted != 9_900_000-3_603_485 {
		t.Fatalf("balance %d: the debit was repeated", posted)
	}
	if n := ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'LOAN_REPAYMENT'`); n != 1 {
		t.Fatalf("%d repayment journals, want 1", n)
	}
	// The client's retry with the same key now reports success.
	st, body = env.Do(t, "POST", "/v1/loans/"+loanID+"/repayments", c.Token, key, map[string]any{"amount_minor": 3_603_485, "currency": "NGN"})
	if st != http.StatusCreated || lendingtest.Decode[repaymentView](t, body).Status != "SUCCESSFUL" {
		t.Fatalf("replay after recovery: %d %s", st, body)
	}
	reconcileClean(t, env)
}
