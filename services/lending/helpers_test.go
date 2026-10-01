package lending_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/identity/identitytest"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/lendingtest"
)

var ctx = context.Background()

// JSON shapes of the API responses the tests read.
type offerView struct {
	OfferID                string `json:"offer_id"`
	Status                 string `json:"status"`
	PrincipalMinor         int64  `json:"principal_minor"`
	OriginationFeeMinor    int64  `json:"origination_fee_minor"`
	NetDisbursementMinor   int64  `json:"net_disbursement_minor"`
	MonthlyRateBps         int    `json:"monthly_rate_bps"`
	InstalmentMinor        int64  `json:"instalment_minor"`
	TotalRepayableMinor    int64  `json:"total_repayable_minor"`
	EffectiveAnnualCostBps int    `json:"effective_annual_cost_bps"`
	DisclosureHash         string `json:"disclosure_hash"`
}

type applicationView struct {
	ApplicationID string     `json:"application_id"`
	Status        string     `json:"status"`
	ReasonCodes   []string   `json:"reason_codes"`
	Offer         *offerView `json:"offer"`
	LoanID        string     `json:"loan_id"`
	Message       string     `json:"message"`
}

type instalmentView struct {
	Seq            int    `json:"seq"`
	DueDate        string `json:"due_date"`
	PrincipalMinor int64  `json:"principal_due_minor"`
	InterestMinor  int64  `json:"interest_due_minor"`
	FeesMinor      int64  `json:"fees_due_minor"`
	PrincipalPaid  int64  `json:"principal_paid_minor"`
	InterestPaid   int64  `json:"interest_paid_minor"`
	FeesPaid       int64  `json:"fees_paid_minor"`
	Paid           bool   `json:"paid"`
}

type loanView struct {
	LoanID          string           `json:"loan_id"`
	Status          string           `json:"status"`
	PrincipalMinor  int64            `json:"principal_minor"`
	DaysPastDue     int              `json:"days_past_due"`
	ArrearsBucket   string           `json:"arrears_bucket"`
	ScheduleVersion int              `json:"schedule_version"`
	Restructured    bool             `json:"restructured"`
	Instalments     []instalmentView `json:"instalments"`
	Payout          *struct {
		Status       string `json:"status"`
		AmountMinor  int64  `json:"amount_minor"`
		AccountLast4 string `json:"account_last4"`
	} `json:"payout"`
}

type repaymentView struct {
	RepaymentID    string `json:"repayment_id"`
	Status         string `json:"status"`
	AppliedMinor   int64  `json:"applied_minor"`
	UnappliedMinor int64  `json:"unapplied_minor"`
	FeesMinor      int64  `json:"fees_minor"`
	InterestMinor  int64  `json:"interest_minor"`
	PrincipalMinor int64  `json:"principal_minor"`
	SettlesLoan    bool   `json:"settles_loan"`
}

type payoffView struct {
	PrincipalMinor int64 `json:"principal_minor"`
	InterestMinor  int64 `json:"interest_minor"`
	FeesMinor      int64 `json:"fees_minor"`
	TotalMinor     int64 `json:"total_minor"`
}

// Standard test application: NGN 100,000 over 3 months, income NGN 300,000.
const (
	amount100k = int64(10_000_000)
	income300k = int64(30_000_000)
)

func applyBody(amount int64, tenor int, income int64) map[string]any {
	return map[string]any{
		"product_id": "personal-loan", "amount_minor": amount, "currency": "NGN", "tenor_months": tenor,
		"stated_monthly_income_minor": income, "consent_credit_check": true,
	}
}

// login refreshes the customer's token (tokens expire as tests move time).
func login(t testing.TB, env *lendingtest.Env, c *identitytest.Customer) {
	t.Helper()
	c.Token = env.Identity.Login(t, c.Phone, c.PIN)
}

// submit submits a standard application and returns its id.
func submit(t testing.TB, env *lendingtest.Env, c identitytest.Customer, amount int64, tenor int, income int64) string {
	t.Helper()
	st, body := env.Do(t, "POST", "/v1/loan-applications", c.Token, lendingtest.Key(), applyBody(amount, tenor, income))
	if st != http.StatusAccepted {
		t.Fatalf("submit: %d %s", st, body)
	}
	return lendingtest.Decode[applicationView](t, body).ApplicationID
}

func assess(t testing.TB, env *lendingtest.Env) {
	t.Helper()
	if err := env.Service.AssessDue(ctx); err != nil {
		t.Fatalf("assess: %v", err)
	}
}

func getApplication(t testing.TB, env *lendingtest.Env, c identitytest.Customer, id string) applicationView {
	t.Helper()
	st, body := env.Do(t, "GET", "/v1/loan-applications/"+id, c.Token, "", nil)
	if st != http.StatusOK {
		t.Fatalf("get application: %d %s", st, body)
	}
	return lendingtest.Decode[applicationView](t, body)
}

func getLoan(t testing.TB, env *lendingtest.Env, c identitytest.Customer, id string) loanView {
	t.Helper()
	st, body := env.Do(t, "GET", "/v1/loans/"+id, c.Token, "", nil)
	if st != http.StatusOK {
		t.Fatalf("get loan: %d %s", st, body)
	}
	return lendingtest.Decode[loanView](t, body)
}

func acceptBody(o offerView, pin string) map[string]any {
	return map[string]any{"disclosure_hash": o.DisclosureHash, "pin": pin, "auto_debit_authorised": false,
		"destination": map[string]any{"type": "DEPOSIT_ACCOUNT"}}
}

// offered registers nothing: it submits, assesses and returns the open offer.
func offered(t testing.TB, env *lendingtest.Env, c identitytest.Customer, amount int64, tenor int) (applicationID string, offer offerView) {
	t.Helper()
	applicationID = submit(t, env, c, amount, tenor, income300k)
	assess(t, env)
	a := getApplication(t, env, c, applicationID)
	if a.Status != "OFFER_READY" || a.Offer == nil {
		t.Fatalf("expected an offer, got %+v", a)
	}
	return applicationID, *a.Offer
}

// disbursed takes a customer all the way to an active loan in their deposit
// account and returns the loan id.
func disbursed(t testing.TB, env *lendingtest.Env, c identitytest.Customer, amount int64, tenor int, autoDebit bool) string {
	t.Helper()
	_, offer := offered(t, env, c, amount, tenor)
	body := acceptBody(offer, c.PIN)
	body["auto_debit_authorised"] = autoDebit
	st, resp := env.Do(t, "POST", "/v1/loan-offers/"+offer.OfferID+"/accept", c.Token, lendingtest.Key(), body)
	if st != http.StatusCreated {
		t.Fatalf("accept: %d %s", st, resp)
	}
	loan := lendingtest.Decode[loanView](t, resp)
	if loan.Status != "ACTIVE" {
		t.Fatalf("loan not active after accept: %+v", loan)
	}
	return loan.LoanID
}

func repay(t testing.TB, env *lendingtest.Env, c identitytest.Customer, loanID string, amount int64) (int, []byte) {
	t.Helper()
	return env.Do(t, "POST", "/v1/loans/"+loanID+"/repayments", c.Token, lendingtest.Key(), map[string]any{"amount_minor": amount, "currency": "NGN"})
}

func balance(t testing.TB, env *lendingtest.Env, c identitytest.Customer) (posted, held int64) {
	t.Helper()
	return env.Ledger.Balance(t, c.AccountCode)
}

func ledgerPosted(t testing.TB, env *lendingtest.Env, code string) int64 {
	t.Helper()
	p, _ := env.Ledger.Balance(t, code)
	return p
}

// advanceTo moves the shared clock to 09:00 Lagos on the given business date.
func advanceTo(t testing.TB, env *lendingtest.Env, date time.Time) {
	t.Helper()
	target := bizdate.StartOf(date).Add(9 * time.Hour)
	d := target.Sub(env.Clock.Now())
	if d < 0 {
		t.Fatalf("cannot move the clock backwards to %s", date.Format(time.DateOnly))
	}
	env.Clock.Advance(d)
}

func today(env *lendingtest.Env) time.Time { return bizdate.Of(env.Clock.Now()) }

func dailyJobs(t testing.TB, env *lendingtest.Env) {
	t.Helper()
	if err := env.Service.RunDailyJobs(ctx); err != nil {
		t.Fatalf("daily jobs: %v", err)
	}
}

// reconcileClean runs ledger reconciliation and fails on any break.
func reconcileClean(t testing.TB, env *lendingtest.Env) {
	t.Helper()
	res, err := env.Service.ReconcileLedger(ctx, time.Time{})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Skipped {
		t.Fatalf("reconciliation skipped: %s", res.SkipReason)
	}
	if res.ControlBreaks != 0 || res.JournalBreaks != 0 {
		open, _ := env.Service.ReconExceptions(ctx, "OPEN")
		t.Fatalf("reconciliation found breaks: %+v exceptions=%+v", res, open)
	}
	env.Ledger.AssertInvariants(t)
}

func count(t testing.TB, env *lendingtest.Env, query string, args ...any) int {
	t.Helper()
	var n int
	if err := env.Pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func ledgerCount(t testing.TB, env *lendingtest.Env, query string, args ...any) int {
	t.Helper()
	var n int
	if err := env.Ledger.Pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("ledger count: %v", err)
	}
	return n
}

var _ = uuid.Nil
var _ = contract.AccountLoansPrincipal
