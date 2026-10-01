package lending_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/identity/identitytest"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/lendingtest"
	sim "bankplatform.internal/simulator"
)

type externalPaymentView struct {
	PaymentID     string  `json:"payment_id"`
	Reference     string  `json:"reference"`
	Provider      string  `json:"provider"`
	Status        string  `json:"status"`
	ExpectedMinor int64   `json:"expected_minor"`
	ReceivedMinor *int64  `json:"received_minor"`
	RepaymentID   *string `json:"repayment_id"`
}

func initiateExternal(t testing.TB, env *lendingtest.Env, c identitytest.Customer, loanID string, amount int64) externalPaymentView {
	t.Helper()
	st, body := env.Do(t, "POST", "/v1/loans/"+loanID+"/external-repayments", c.Token, lendingtest.Key(),
		map[string]any{"amount_minor": amount, "currency": "NGN"})
	if st != http.StatusCreated {
		t.Fatalf("initiate external repayment: %d %s", st, body)
	}
	v := lendingtest.Decode[externalPaymentView](t, body)
	if v.Status != "AWAITING_PAYMENT" || v.ExpectedMinor != amount || v.Reference == "" {
		t.Fatalf("initiated payment: %+v", v)
	}
	return v
}

func getExternal(t testing.TB, env *lendingtest.Env, c identitytest.Customer, loanID, paymentID string) externalPaymentView {
	t.Helper()
	st, body := env.Do(t, "GET", "/v1/loans/"+loanID+"/external-repayments/"+paymentID, c.Token, "", nil)
	if st != http.StatusOK {
		t.Fatalf("get external repayment: %d %s", st, body)
	}
	return lendingtest.Decode[externalPaymentView](t, body)
}

func paymentRow(t testing.TB, env *lendingtest.Env, paymentID string) domain.ExternalPayment {
	t.Helper()
	p, err := env.Store.GetExternalPayment(ctx, uuid.MustParse(paymentID))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// notify delivers the provider's webhook for a payment reference.
func notify(t testing.TB, env *lendingtest.Env, reference string) {
	t.Helper()
	if err := env.Sim.SendPaymentWebhook(ctx, uuid.NewString(), reference); err != nil {
		t.Fatalf("payment webhook: %v", err)
	}
}

// poll moves time past the longest retry interval and runs the poller.
func poll(t testing.TB, env *lendingtest.Env) {
	t.Helper()
	env.Clock.Advance(6 * time.Minute)
	if err := env.Service.DriveDueExternalPayments(ctx); err != nil {
		t.Logf("external payment driver: %v", err) // injected faults surface here
	}
}

func clearing(t testing.TB, env *lendingtest.Env) int64 {
	t.Helper()
	return ledgerPosted(t, env, contract.AccountCollectionsClearing)
}

func creditJournals(t testing.TB, env *lendingtest.Env, paymentID string) int {
	t.Helper()
	return ledgerCount(t, env, `SELECT count(*) FROM ledger.journals WHERE journal_type = 'EXTERNAL_COLLECTION' AND op_id = $1`, uuid.MustParse(paymentID))
}

// dueLoan returns a customer with an active loan whose first instalment is
// due today, and that instalment's amount.
func dueLoan(t testing.TB, env *lendingtest.Env) (c identitytest.Customer, loanID string, instalment int64) {
	t.Helper()
	c = env.Identity.Register(t)
	loanID = disbursed(t, env, c, amount100k, 3, false)
	loan := getLoan(t, env, c, loanID)
	if due := firstDue(t, loan); due.After(today(env)) {
		advanceTo(t, env, due)
	}
	login(t, env, &c)
	dailyJobs(t, env)
	inst := getLoan(t, env, c, loanID).Instalments[0]
	return c, loanID, inst.PrincipalMinor + inst.InterestMinor + inst.FeesMinor
}

// The core property: money is credited when, and only when, the provider
// itself confirms the payment. A webhook alone credits nothing.
func TestExternalRepaymentIsCreditedOnlyOnProviderConfirmation(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c, loanID, amount := dueLoan(t, env)
	depositBefore, _ := balance(t, env, c)

	p := initiateExternal(t, env, c, loanID, amount)
	if p.Provider != "collection-simulator" || clearing(t, env) != 0 {
		t.Fatalf("initiation must move no money: %+v clearing %d", p, clearing(t, env))
	}

	// A correctly signed webhook arrives, but the provider has no record of
	// any payment under the reference. Nothing may be credited.
	notify(t, env, p.Reference)
	if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalInitiated || clearing(t, env) != 0 {
		t.Fatalf("a webhook without a provider record changed things: state %s clearing %d", got.State, clearing(t, env))
	}

	// The customer pays; the provider now has the payment; the webhook
	// triggers verification.
	env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)
	notify(t, env, p.Reference)

	got := getExternal(t, env, c, loanID, p.PaymentID)
	if got.Status != "APPLIED" || got.ReceivedMinor == nil || *got.ReceivedMinor != amount || got.RepaymentID == nil {
		t.Fatalf("after confirmation: %+v", got)
	}
	if n := clearing(t, env); n != amount {
		t.Fatalf("collections clearing %d, want %d", n, amount)
	}
	// Credited and then repaid: the customer's own balance is unchanged.
	if after, _ := balance(t, env, c); after != depositBefore {
		t.Fatalf("deposit %d, want %d", after, depositBefore)
	}
	if inst := getLoan(t, env, c, loanID).Instalments[0]; !inst.Paid {
		t.Fatalf("instalment not paid: %+v", inst)
	}
	if n := creditJournals(t, env, p.PaymentID); n != 1 {
		t.Fatalf("%d credit journals", n)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.repayments WHERE repayment_id = $1 AND source = 'EXTERNAL' AND state = 'ALLOCATED'`, uuid.MustParse(p.PaymentID)); n != 1 {
		t.Fatalf("%d external repayments", n)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.outbox WHERE event_type = 'loan.external_payment.applied'`); n != 1 {
		t.Fatalf("%d applied events in the outbox", n)
	}
	reconcileClean(t, env)
}

// The provider delivers the same event repeatedly, different events with the
// same news arrive, and the poller runs, all at once. One credit, one
// repayment.
func TestDuplicateAndConcurrentPaymentNotificationsCreditOnce(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c, loanID, amount := dueLoan(t, env)
	depositBefore, _ := balance(t, env, c)
	p := initiateExternal(t, env, c, loanID, amount)
	env.Sim.SetPayment(p.Reference, amount, "NGN", sim.StatusSuccess)

	sameEvent := uuid.NewString()
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_ = env.Sim.SendPaymentWebhook(ctx, sameEvent, p.Reference)
			case 1:
				_ = env.Sim.SendPaymentWebhook(ctx, uuid.NewString(), p.Reference)
			default:
				_ = env.Service.DriveDueExternalPayments(ctx)
			}
		}()
	}
	wg.Wait()
	// Whatever the interleaving left unfinished, the poller completes.
	for range 3 {
		poll(t, env)
	}

	if got := paymentRow(t, env, p.PaymentID); got.State != domain.ExternalApplied {
		t.Fatalf("payment %s (%s)", got.State, got.LastCode)
	}
	if n := creditJournals(t, env, p.PaymentID); n != 1 {
		t.Fatalf("%d credit journals, want exactly 1", n)
	}
	if n := clearing(t, env); n != amount {
		t.Fatalf("clearing %d, want %d (credited more than once?)", n, amount)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.repayments WHERE loan_id = $1`, uuid.MustParse(loanID)); n != 1 {
		t.Fatalf("%d repayments, want 1", n)
	}
	if after, _ := balance(t, env, c); after != depositBefore {
		t.Fatalf("deposit %d, want %d", after, depositBefore)
	}
	// 8 deliveries of one event id are stored once; 8 distinct ids once each.
	if n := count(t, env, `SELECT count(*) FROM lending.external_payment_events WHERE reference = $1`, p.Reference); n != 9 {
		t.Fatalf("%d webhook events recorded, want 9", n)
	}
	reconcileClean(t, env)
}

func TestExternalRepaymentAPI(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c, loanID, amount := dueLoan(t, env)
	path := "/v1/loans/" + loanID + "/external-repayments"
	body := map[string]any{"amount_minor": amount, "currency": "NGN"}

	t.Run("requires authentication and an idempotency key", func(t *testing.T) {
		if st, _ := env.Do(t, "POST", path, "", lendingtest.Key(), body); st != http.StatusUnauthorized {
			t.Fatalf("no token: %d", st)
		}
		if st, b := env.Do(t, "POST", path, c.Token, "", body); st != http.StatusBadRequest || lendingtest.ErrorCode(t, b) != "VALIDATION_FAILED" {
			t.Fatalf("no key: %d %s", st, b)
		}
	})

	t.Run("validates amount and currency", func(t *testing.T) {
		if st, b := env.Do(t, "POST", path, c.Token, lendingtest.Key(), map[string]any{"amount_minor": 0, "currency": "NGN"}); st != http.StatusBadRequest {
			t.Fatalf("zero amount: %d %s", st, b)
		}
		if st, b := env.Do(t, "POST", path, c.Token, lendingtest.Key(), map[string]any{"amount_minor": amount, "currency": "USD"}); st != http.StatusBadRequest {
			t.Fatalf("wrong currency: %d %s", st, b)
		}
	})

	t.Run("a retry returns the same payment; a changed body is refused", func(t *testing.T) {
		key := lendingtest.Key()
		st, b := env.Do(t, "POST", path, c.Token, key, body)
		first := lendingtest.Decode[externalPaymentView](t, b)
		if st != http.StatusCreated {
			t.Fatalf("first: %d %s", st, b)
		}
		st, b = env.Do(t, "POST", path, c.Token, key, body)
		if again := lendingtest.Decode[externalPaymentView](t, b); st != http.StatusCreated || again.PaymentID != first.PaymentID || again.Reference != first.Reference {
			t.Fatalf("retry: %d %+v, want payment %s", st, again, first.PaymentID)
		}
		if st, b := env.Do(t, "POST", path, c.Token, key, map[string]any{"amount_minor": amount + 1, "currency": "NGN"}); st != http.StatusUnprocessableEntity || lendingtest.ErrorCode(t, b) != "IDEMPOTENCY_KEY_REUSED" {
			t.Fatalf("changed body: %d %s", st, b)
		}
		if n := count(t, env, `SELECT count(*) FROM lending.external_payments WHERE reference = $1`, first.Reference); n != 1 {
			t.Fatalf("%d payments for one key", n)
		}
	})

	t.Run("another customer cannot see or pay towards the loan", func(t *testing.T) {
		mine := initiateExternal(t, env, c, loanID, amount)
		other := env.Identity.Register(t)
		if st, b := env.Do(t, "POST", path, other.Token, lendingtest.Key(), body); st != http.StatusNotFound {
			t.Fatalf("foreign loan: %d %s", st, b)
		}
		if st, b := env.Do(t, "GET", path+"/"+mine.PaymentID, other.Token, "", nil); st != http.StatusNotFound {
			t.Fatalf("foreign payment: %d %s", st, b)
		}
	})

	t.Run("a webhook for a reference we never issued raises an exception", func(t *testing.T) {
		notify(t, env, "rp-not-ours")
		open, _ := env.Service.ReconExceptions(ctx, "OPEN")
		for _, e := range open {
			if e.Kind == domain.ExceptionUnknownPayment && e.EntityID == "rp-not-ours" {
				return
			}
		}
		t.Fatalf("no exception for the unknown reference: %+v", open)
	})
}
