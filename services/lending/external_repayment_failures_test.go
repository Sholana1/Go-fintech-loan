package lending_test

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/lendingtest"
	sim "bankplatform.internal/simulator"
)

func TestForgedPaymentWebhooksAreRejected(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c, loanID, amount := dueLoan(t, env)
	p := initiateExternal(t, env, c, loanID, amount)
	// The provider really has the payment: if a forged webhook were
	// accepted, the customer would be credited.
	env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)

	body := []byte(fmt.Sprintf(`{"event_id":"%s","type":"payment.updated","reference":"%s"}`, uuid.NewString(), p.Reference))
	ts := strconv.FormatInt(env.Clock.Now().Unix(), 10)

	if err := env.Sim.PostPaymentWebhook(ctx, body, ts, "deadbeef"); err == nil {
		t.Fatal("a forged webhook was accepted")
	}
	if err := env.Sim.PostPaymentWebhook(ctx, body, ts, sim.Sign("not-the-secret", ts, body)); err == nil {
		t.Fatal("a webhook signed with the wrong secret was accepted")
	}
	old := strconv.FormatInt(env.Clock.Now().Add(-time.Hour).Unix(), 10)
	if err := env.Sim.PostPaymentWebhook(ctx, body, old, ""); err == nil {
		t.Fatal("a stale webhook was accepted")
	}
	other := []byte(fmt.Sprintf(`{"event_id":"%s","type":"payment.updated","reference":"rp-someone-else"}`, uuid.NewString()))
	if err := env.Sim.PostPaymentWebhook(ctx, other, ts, sim.Sign("sim-callback-secret", ts, body)); err == nil {
		t.Fatal("a tampered webhook was accepted")
	}

	if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalInitiated {
		t.Fatalf("a rejected webhook moved the payment to %s", got.State)
	}
	if clearing(t, env) != 0 {
		t.Fatal("a rejected webhook credited money")
	}
	if n := env.Metrics.Count("callback:rejected"); n != 4 {
		t.Fatalf("%d rejected webhooks counted, want 4", n)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.external_payment_events`); n != 0 {
		t.Fatalf("%d events stored from unauthenticated webhooks", n)
	}
}

// Every way the provider can fail to answer is "unknown": the payment is
// neither credited nor failed, and it completes once the provider answers.
func TestProviderFailuresDuringVerificationAreUnknownNotFailed(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	for _, fault := range []string{"timeout", "http500", "malformed"} {
		t.Run(fault, func(t *testing.T) {
			c, loanID, amount := dueLoan(t, env)
			before := clearing(t, env)
			p := initiateExternal(t, env, c, loanID, amount)
			env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)
			env.Sim.SetPaymentFault(p.Reference, fault)

			notify(t, env, p.Reference)
			got := paymentRow(t, env, p.PaymentID)
			if got.State != domain.ExternalInitiated || got.LastCode != "VERIFY_UNAVAILABLE" {
				t.Fatalf("after a %s: state %s code %s", fault, got.State, got.LastCode)
			}
			if clearing(t, env) != before {
				t.Fatalf("credited after a %s", fault)
			}

			// The provider recovers; the poller (no webhook this time) finishes.
			env.Sim.SetPaymentFault(p.Reference, "")
			poll(t, env)
			if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalApplied {
				t.Fatalf("after recovery: %s (%s)", got.State, got.LastCode)
			}
			if clearing(t, env) != before+amount || creditJournals(t, env, p.PaymentID) != 1 {
				t.Fatalf("after recovery: clearing %d journals %d", clearing(t, env)-before, creditJournals(t, env, p.PaymentID))
			}
		})
	}
	reconcileClean(t, env)
}

func TestFailedAndExpiredPaymentsAreStillCreditedIfTheMoneyArrivesLater(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})

	t.Run("provider says failed, then proves success", func(t *testing.T) {
		c, loanID, amount := dueLoan(t, env)
		before := clearing(t, env)
		p := initiateExternal(t, env, c, loanID, amount)

		env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusFailed)
		notify(t, env, p.Reference)
		if got := getExternal(t, env, c, loanID, p.PaymentID); got.Status != "FAILED" || clearing(t, env) != before {
			t.Fatalf("after a failed payment: %+v", got)
		}

		env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)
		notify(t, env, p.Reference)
		if got := getExternal(t, env, c, loanID, p.PaymentID); got.Status != "APPLIED" || clearing(t, env) != before+amount {
			t.Fatalf("after the late success: %+v clearing +%d", got, clearing(t, env)-before)
		}
	})

	t.Run("never paid in time, then paid late", func(t *testing.T) {
		c, loanID, amount := dueLoan(t, env)
		before := clearing(t, env)
		p := initiateExternal(t, env, c, loanID, amount)

		env.Clock.Advance(env.Config.ExternalPaymentValidity + time.Minute)
		if err := env.Service.DriveDueExternalPayments(ctx); err != nil {
			t.Fatal(err)
		}
		if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalExpired {
			t.Fatalf("after the validity window: %s", got.State)
		}
		// An expired payment is off the work queue: the poller no longer asks.
		queries := func() int { pay, _ := env.Sim.Payment(p.Reference); return pay.QueryCount }
		poll(t, env)
		if n := queries(); n != 0 {
			t.Fatalf("an expired payment was still polled (%d queries)", n)
		}

		env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)
		notify(t, env, p.Reference)
		if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalApplied || clearing(t, env) != before+amount {
			t.Fatalf("late payment: %s clearing +%d", got.State, clearing(t, env)-before)
		}
	})
	reconcileClean(t, env)
}

func TestWhatIsCreditedIsWhatTheProviderCollected(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})

	t.Run("less than announced", func(t *testing.T) {
		c, loanID, amount := dueLoan(t, env)
		before := clearing(t, env)
		p := initiateExternal(t, env, c, loanID, amount)
		env.Sim.SetPayment(p.Reference, 100_000, "NGN", sim.StatusSuccess)
		notify(t, env, p.Reference)

		got := paymentRow(t, env, p.PaymentID)
		if got.State != domain.ExternalApplied || got.VerifiedMinor == nil || *got.VerifiedMinor != 100_000 {
			t.Fatalf("payment %s verified %v", got.State, got.VerifiedMinor)
		}
		if clearing(t, env) != before+100_000 {
			t.Fatalf("clearing +%d, want 100000", clearing(t, env)-before)
		}
		if inst := getLoan(t, env, c, loanID).Instalments[0]; inst.Paid || inst.InterestPaid != 100_000 {
			t.Fatalf("instalment after a short payment: %+v", inst)
		}
	})

	t.Run("more than the loan can take stays in the customer's account", func(t *testing.T) {
		c, loanID, amount := dueLoan(t, env)
		depositBefore, _ := balance(t, env, c)
		before := clearing(t, env)
		paid := amount + 12_345
		p := initiateExternal(t, env, c, loanID, paid)
		env.Sim.SetPayment(p.Reference, paid, "NGN", sim.StatusSuccess)
		notify(t, env, p.Reference)

		if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalApplied {
			t.Fatalf("payment %s (%s)", got.State, got.LastCode)
		}
		var applied int64
		if err := env.Pool.QueryRow(ctx, `SELECT applied_minor FROM lending.repayments WHERE repayment_id = $1`, uuid.MustParse(p.PaymentID)).Scan(&applied); err != nil {
			t.Fatal(err)
		}
		after, _ := balance(t, env, c)
		if clearing(t, env) != before+paid || after-depositBefore != paid-applied || applied > paid {
			t.Fatalf("paid %d applied %d deposit +%d clearing +%d", paid, applied, after-depositBefore, clearing(t, env)-before)
		}
	})

	t.Run("a payment in another currency is not credited by rule", func(t *testing.T) {
		c, loanID, amount := dueLoan(t, env)
		before := clearing(t, env)
		p := initiateExternal(t, env, c, loanID, amount)
		env.Sim.SetPayment(p.Reference, amount, "USD", sim.StatusSuccess)
		notify(t, env, p.Reference)

		if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalReview || clearing(t, env) != before {
			t.Fatalf("payment %s clearing +%d", got.State, clearing(t, env)-before)
		}
		open, _ := env.Service.ReconExceptions(ctx, "OPEN")
		for _, e := range open {
			if e.Kind == domain.ExceptionPaymentCurrency && e.EntityID == p.PaymentID {
				return
			}
		}
		t.Fatalf("no exception for the currency mismatch: %+v", open)
	})
}

// The ledger fails at the worst moments. The credit is posted exactly once.
func TestExternalPaymentCreditSurvivesLedgerFailures(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})

	t.Run("ledger unreachable: nothing posted, retried later", func(t *testing.T) {
		c, loanID, amount := dueLoan(t, env)
		before := clearing(t, env)
		p := initiateExternal(t, env, c, loanID, amount)
		env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)

		env.Faults.FailBefore("Post", 1)
		notify(t, env, p.Reference)
		if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalVerified || clearing(t, env) != before {
			t.Fatalf("with the ledger down: %s clearing +%d", got.State, clearing(t, env)-before)
		}
		poll(t, env)
		if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalApplied || creditJournals(t, env, p.PaymentID) != 1 {
			t.Fatalf("after recovery: %s, %d journals", got.State, creditJournals(t, env, p.PaymentID))
		}
	})

	t.Run("ledger posted but the response was lost: not posted again", func(t *testing.T) {
		c, loanID, amount := dueLoan(t, env)
		before := clearing(t, env)
		p := initiateExternal(t, env, c, loanID, amount)
		env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)

		env.Faults.FailAfter("Post", 1)
		notify(t, env, p.Reference)
		// The journal exists; lending does not know it yet.
		if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalVerified || clearing(t, env) != before+amount {
			t.Fatalf("after the lost response: %s clearing +%d", got.State, clearing(t, env)-before)
		}
		prevented := env.Metrics.Count("duplicate:EXTERNAL_PAYMENT")
		poll(t, env)
		if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalApplied {
			t.Fatalf("after retry: %s (%s)", got.State, got.LastCode)
		}
		if clearing(t, env) != before+amount || creditJournals(t, env, p.PaymentID) != 1 {
			t.Fatalf("credited twice: clearing +%d, %d journals", clearing(t, env)-before, creditJournals(t, env, p.PaymentID))
		}
		if env.Metrics.Count("duplicate:EXTERNAL_PAYMENT") != prevented+1 {
			t.Fatal("the repeated posting was not recognised as a duplicate")
		}
	})
	reconcileClean(t, env)
}

// The customer settles the loan from their account while an external payment
// is on its way. The money still arrives and stays theirs.
func TestExternalPaymentForASettledLoanStaysInTheCustomersAccount(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c, loanID, amount := dueLoan(t, env)
	p := initiateExternal(t, env, c, loanID, amount)

	env.Ledger.Fund(t, c.AccountCode, 2_000_000)
	available, _ := balance(t, env, c)
	if st, body := repay(t, env, c, loanID, available); st != 201 || !lendingtest.Decode[repaymentView](t, body).SettlesLoan {
		t.Fatalf("settlement: %d %s", st, body)
	}
	depositBefore, _ := balance(t, env, c)

	env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)
	notify(t, env, p.Reference)

	got := getExternal(t, env, c, loanID, p.PaymentID)
	if got.Status != "CREDITED_TO_ACCOUNT" {
		t.Fatalf("payment for a settled loan: %+v", got)
	}
	if after, _ := balance(t, env, c); after-depositBefore != amount {
		t.Fatalf("deposit +%d, want the full payment %d", after-depositBefore, amount)
	}
	if st, body := env.Do(t, "POST", "/v1/loans/"+loanID+"/external-repayments", c.Token, lendingtest.Key(),
		map[string]any{"amount_minor": amount, "currency": "NGN"}); st != 409 || lendingtest.ErrorCode(t, body) != "LOAN_NOT_REPAYABLE" {
		t.Fatalf("initiating against a closed loan: %d %s", st, body)
	}
	reconcileClean(t, env)
}
