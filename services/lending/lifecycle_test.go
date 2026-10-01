package lending_test

import (
	"net/http"
	"testing"
	"time"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/lendingtest"
)

// The complete journey of one loan: apply, assess, offer, accept, disburse,
// accrue interest daily, repay each instalment on its due date, close; with
// the ledger and the loan book reconciled at every stage.
func TestLoanLifecycleFromApplicationToClosure(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	start := today(env)

	// --- Application and offer.
	applicationID, offer := offered(t, env, c, amount100k, 3)
	// Band A (score 720) is 4% per month in the illustrative product.
	if offer.PrincipalMinor != amount100k || offer.MonthlyRateBps != 400 || offer.OriginationFeeMinor != 100_000 ||
		offer.NetDisbursementMinor != 9_900_000 || offer.InstalmentMinor != 3_603_485 || len(offer.DisclosureHash) != 64 {
		t.Fatalf("unexpected offer %+v", offer)
	}

	// --- Acceptance and disbursement (T2 -> T3).
	st, body := env.Do(t, "POST", "/v1/loan-offers/"+offer.OfferID+"/accept", c.Token, lendingtest.Key(), acceptBody(offer, c.PIN))
	if st != http.StatusCreated {
		t.Fatalf("accept: %d %s", st, body)
	}
	loan := lendingtest.Decode[loanView](t, body)
	if loan.Status != "ACTIVE" || len(loan.Instalments) != 3 {
		t.Fatalf("loan %+v", loan)
	}
	if posted, _ := balance(t, env, c); posted != 9_900_000 {
		t.Fatalf("customer received %d, want the net disbursement 9,900,000", posted)
	}
	// Accounting of the disbursement, from the bank's side.
	if got := ledgerPosted(t, env, contract.AccountLoansPrincipal); got != 10_000_000 {
		t.Fatalf("loans receivable %d, want 10,000,000", got)
	}
	if got := ledgerPosted(t, env, contract.AccountFeeIncome); got != 100_000 {
		t.Fatalf("fee income %d, want 100,000", got)
	}
	if a := getApplication(t, env, c, applicationID); a.Status != "DISBURSED" || a.LoanID != loan.LoanID {
		t.Fatalf("application after disbursement: %+v", a)
	}
	reconcileClean(t, env)

	// The customer needs money to repay with (inbound transfers are a later
	// product; the development funding account stands in for one).
	env.Ledger.Fund(t, c.AccountCode, 2_000_000)

	// --- Service the loan: each month, run the daily jobs up to the due date
	// and pay the instalment on the day.
	var interestPaid int64
	for k, inst := range loan.Instalments {
		due, _ := time.Parse(time.DateOnly, inst.DueDate)
		if want := bizdate.AddMonths(start, k+1); !due.Equal(want) {
			t.Fatalf("instalment %d due %s, want %s", k+1, inst.DueDate, want.Format(time.DateOnly))
		}
		advanceTo(t, env, due)
		login(t, env, &c)
		dailyJobs(t, env)

		// By the due date exactly this instalment's interest has been earned
		// and recognised: the receivable equals what is about to be paid.
		if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != inst.InterestMinor {
			t.Fatalf("month %d: interest receivable %d, want %d", k+1, got, inst.InterestMinor)
		}

		st, body := repay(t, env, c, loan.LoanID, inst.PrincipalMinor+inst.InterestMinor)
		if st != http.StatusCreated {
			t.Fatalf("repay %d: %d %s", k+1, st, body)
		}
		rep := lendingtest.Decode[repaymentView](t, body)
		if rep.Status != "SUCCESSFUL" || rep.InterestMinor != inst.InterestMinor || rep.PrincipalMinor != inst.PrincipalMinor || rep.UnappliedMinor != 0 {
			t.Fatalf("repayment %d allocation %+v", k+1, rep)
		}
		if rep.SettlesLoan != (k == 2) {
			t.Fatalf("repayment %d settles=%v", k+1, rep.SettlesLoan)
		}
		interestPaid += inst.InterestMinor
		reconcileClean(t, env)
	}

	final := getLoan(t, env, c, loan.LoanID)
	if final.Status != "CLOSED" || final.DaysPastDue != 0 {
		t.Fatalf("final loan %+v", final)
	}
	for _, i := range final.Instalments {
		if !i.Paid {
			t.Fatalf("instalment %d not paid at closure", i.Seq)
		}
	}

	// Closing position: nothing receivable, income equals interest plus fee,
	// and the customer's balance is what they were given, plus what they
	// brought, minus what they repaid.
	if got := ledgerPosted(t, env, contract.AccountLoansPrincipal); got != 0 {
		t.Fatalf("principal receivable at closure %d", got)
	}
	if got := ledgerPosted(t, env, contract.AccountLoansInterestReceivable); got != 0 {
		t.Fatalf("interest receivable at closure %d", got)
	}
	if got := ledgerPosted(t, env, contract.AccountInterestIncome); got != interestPaid {
		t.Fatalf("interest income %d, want %d", got, interestPaid)
	}
	wantBalance := int64(9_900_000+2_000_000) - 10_000_000 - interestPaid
	if posted, held := balance(t, env, c); posted != wantBalance || held != 0 {
		t.Fatalf("closing balance %d (held %d), want %d", posted, held, wantBalance)
	}
	if offer.TotalRepayableMinor != 10_000_000+interestPaid {
		t.Fatalf("disclosed total repayable %d differs from what was actually repaid %d", offer.TotalRepayableMinor, 10_000_000+interestPaid)
	}

	// The loan's history was published, in order, on one partition key.
	rows, err := env.Pool.Query(ctx, `SELECT event_type FROM lending.outbox WHERE partition_key = $1 ORDER BY outbox_id`, applicationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var e string
		_ = rows.Scan(&e)
		events = append(events, e)
	}
	want := []string{"loan.application.submitted", "loan.application.offered", "loan.offer.accepted", "loan.disbursed",
		"loan.repayment.allocated", "loan.repayment.allocated", "loan.closed"}
	if len(events) != len(want) {
		t.Fatalf("events %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events %v, want %v", events, want)
		}
	}

	// A statement is available and adds up.
	st, body = env.Do(t, "GET", "/v1/loans/"+loan.LoanID+"/statement", c.Token, "", nil)
	if st != http.StatusOK {
		t.Fatalf("statement: %d %s", st, body)
	}
	stmt := lendingtest.Decode[struct {
		Repayments []repaymentView `json:"repayments"`
		Totals     struct {
			PrincipalOutstanding int64 `json:"principal_outstanding_minor"`
			PrincipalPaid        int64 `json:"principal_paid_minor"`
			InterestPaid         int64 `json:"interest_paid_minor"`
		} `json:"totals"`
	}](t, body)
	if len(stmt.Repayments) != 3 || stmt.Totals.PrincipalOutstanding != 0 || stmt.Totals.PrincipalPaid != 10_000_000 || stmt.Totals.InterestPaid != interestPaid {
		t.Fatalf("statement %+v", stmt)
	}
}
