package lending_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/services/identity/identitytest"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/lendingtest"
	sim "bankplatform.internal/simulator"
)

// registerWithBVN registers a customer whose BVN suffix selects a simulated
// bureau behaviour (see package sim).
func registerWithBVN(t *testing.T, env *lendingtest.Env, suffix string) identitytest.Customer {
	t.Helper()
	c := identitytest.Customer{Phone: env.Identity.NextPhone(), PIN: "482915", BVN: env.Identity.NextBVN(suffix), FullName: "Ada Test Customer"}
	st, body := env.Identity.Post(t, "/v1/customers", map[string]string{
		"phone": c.Phone, "full_name": c.FullName, "date_of_birth": "1990-05-17", "bvn": c.BVN, "pin": c.PIN})
	if st != http.StatusCreated {
		t.Fatalf("register: %d %s", st, body)
	}
	var reg struct {
		CustomerID string `json:"customer_id"`
	}
	_ = json.Unmarshal(body, &reg)
	c.ID = uuid.MustParse(reg.CustomerID)
	c.AccountCode = "CUST:" + reg.CustomerID + ":MAIN"
	c.Token = env.Identity.Login(t, c.Phone, c.PIN)
	return c
}

func TestSubmissionValidation(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)

	cases := []struct {
		name   string
		mutate func(m map[string]any)
		key    string
		status int
		code   string
	}{
		{"no idempotency key", func(map[string]any) {}, "", 400, "VALIDATION_FAILED"},
		{"below minimum", func(m map[string]any) { m["amount_minor"] = 900_000 }, lendingtest.Key(), 400, "VALIDATION_FAILED"},
		{"above maximum", func(m map[string]any) { m["amount_minor"] = 60_000_000 }, lendingtest.Key(), 400, "VALIDATION_FAILED"},
		{"not a whole step", func(m map[string]any) { m["amount_minor"] = 10_000_001 }, lendingtest.Key(), 400, "VALIDATION_FAILED"},
		{"tenor not offered", func(m map[string]any) { m["tenor_months"] = 4 }, lendingtest.Key(), 400, "VALIDATION_FAILED"},
		{"wrong currency", func(m map[string]any) { m["currency"] = "USD" }, lendingtest.Key(), 400, "VALIDATION_FAILED"},
		{"no income", func(m map[string]any) { m["stated_monthly_income_minor"] = 0 }, lendingtest.Key(), 400, "VALIDATION_FAILED"},
		{"no consent", func(m map[string]any) { m["consent_credit_check"] = false }, lendingtest.Key(), 400, "VALIDATION_FAILED"},
		{"unknown product", func(m map[string]any) { m["product_id"] = "mortgage" }, lendingtest.Key(), 400, "VALIDATION_FAILED"},
		{"unknown field", func(m map[string]any) { m["contacts"] = []string{"+2348000000000"} }, lendingtest.Key(), 400, "INVALID_JSON"},
		{"float amount", func(m map[string]any) { m["amount_minor"] = 100000.5 }, lendingtest.Key(), 400, "INVALID_JSON"},
	}
	for _, tc := range cases {
		body := applyBody(amount100k, 3, income300k)
		tc.mutate(body)
		st, resp := env.Do(t, "POST", "/v1/loan-applications", c.Token, tc.key, body)
		if st != tc.status || lendingtest.ErrorCode(t, resp) != tc.code {
			t.Errorf("%s: got %d %s, want %d %s", tc.name, st, resp, tc.status, tc.code)
		}
	}
	if n := count(t, env, `SELECT count(*) FROM lending.applications`); n != 0 {
		t.Fatalf("%d applications were created by invalid requests", n)
	}
}

// Many simultaneous, distinct submissions by one customer: exactly one
// application is created; the rest are told one is already open.
func TestConcurrentDuplicateSubmissionsCreateOneApplication(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)

	var accepted, conflict atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body := env.Do(t, "POST", "/v1/loan-applications", c.Token, lendingtest.Key(), applyBody(amount100k, 3, income300k))
			switch {
			case st == http.StatusAccepted:
				accepted.Add(1)
			case st == http.StatusConflict && lendingtest.ErrorCode(t, body) == "APPLICATION_ALREADY_OPEN":
				conflict.Add(1)
			default:
				t.Errorf("unexpected %d %s", st, body)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 || conflict.Load() != 15 {
		t.Fatalf("accepted=%d conflict=%d, want 1/15", accepted.Load(), conflict.Load())
	}
	if n := count(t, env, `SELECT count(*) FROM lending.applications`); n != 1 {
		t.Fatalf("%d applications exist, want 1", n)
	}
}

// The same idempotency key replays the original application; the same key
// with a different body is refused.
func TestSubmissionIdempotency(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	key := lendingtest.Key()

	ids := make([]string, 12)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, body := env.Do(t, "POST", "/v1/loan-applications", c.Token, key, applyBody(amount100k, 3, income300k))
			if st != http.StatusAccepted {
				t.Errorf("submit %d: %d %s", i, st, body)
				return
			}
			ids[i] = lendingtest.Decode[applicationView](t, body).ApplicationID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("the same key returned different applications: %v", ids)
		}
	}
	if got := env.Metrics.Count("replay:POST /v1/loan-applications"); got != 11 {
		t.Fatalf("replays counted %d, want 11", got)
	}

	st, body := env.Do(t, "POST", "/v1/loan-applications", c.Token, key, applyBody(amount100k+100_000, 3, income300k))
	if st != http.StatusUnprocessableEntity || lendingtest.ErrorCode(t, body) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("same key, different body: %d %s", st, body)
	}
	// Another customer may use the same key value: keys are scoped per principal.
	other := env.Identity.Register(t)
	if st, body := env.Do(t, "POST", "/v1/loan-applications", other.Token, key, applyBody(amount100k, 3, income300k)); st != http.StatusAccepted {
		t.Fatalf("other customer with the same key: %d %s", st, body)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.applications`); n != 2 {
		t.Fatalf("%d applications, want 2", n)
	}
}

func TestAssessmentOutcomes(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})

	cases := []struct {
		name    string
		suffix  string
		amount  int64
		income  int64
		status  string
		reason  string
		approve int64
		rate    int
	}{
		{"band A approval", "4242", amount100k, income300k, "OFFER_READY", "", amount100k, 400},
		{"band B pricing", "4201", amount100k, income300k, "OFFER_READY", "", amount100k, 600},
		{"band C pricing", "4202", amount100k, income300k, "OFFER_READY", "", amount100k, 800},
		{"score below minimum", "4203", amount100k, income300k, "DECLINED", domain.ReasonBureauScoreLow, 0, 0},
		{"recent delinquency", "4205", amount100k, income300k, "DECLINED", domain.ReasonBureauDelinquency, 0, 0},
		{"thin file is referred", "4204", amount100k, income300k, "UNDER_REVIEW", domain.ReasonThinFile, 0, 0},
		// Obligations of 90,000/month against a 40% budget on 300,000 leave
		// 30,000/month: the amount is reduced to what that can service.
		{"reduced for affordability", "4206", 30_000_000, income300k, "OFFER_READY", domain.ReasonAmountReduced, 8_300_000, 400},
		{"unaffordable", "4206", 30_000_000, 22_000_000, "DECLINED", domain.ReasonAffordability, 0, 0},
		{"above auto-approval limit", "4242", 40_000_000, 500_000_000, "UNDER_REVIEW", domain.ReasonAboveAutoApprove, 0, 0},
	}
	for _, tc := range cases {
		c := registerWithBVN(t, env, tc.suffix)
		id := submit(t, env, c, tc.amount, 3, tc.income)
		assess(t, env)
		a := getApplication(t, env, c, id)
		if a.Status != tc.status {
			t.Errorf("%s: status %s (%v), want %s", tc.name, a.Status, a.ReasonCodes, tc.status)
			continue
		}
		if tc.reason != "" && !slices.Contains(a.ReasonCodes, tc.reason) {
			t.Errorf("%s: reasons %v, want %s", tc.name, a.ReasonCodes, tc.reason)
		}
		if tc.status == "OFFER_READY" {
			if a.Offer == nil || a.Offer.PrincipalMinor != tc.approve || a.Offer.MonthlyRateBps != tc.rate {
				t.Errorf("%s: offer %+v, want principal %d at %d bps", tc.name, a.Offer, tc.approve, tc.rate)
			}
		} else if a.Offer != nil {
			t.Errorf("%s: an offer exists for a non-approved application", tc.name)
		}
	}
}

// A customer who is ruled out before the bureau is consulted is declined
// without a bureau enquiry being made at all.
func TestIneligibleApplicationsDoNotTriggerABureauEnquiry(t *testing.T) {
	// Identity grants tier 1; the product requires tier 2.
	env := lendingtest.Start(t, lendingtest.Options{KYCTier: 1})
	c := env.Identity.Register(t)
	id := submit(t, env, c, amount100k, 3, income300k)
	assess(t, env)

	a := getApplication(t, env, c, id)
	if a.Status != "DECLINED" || !slices.Contains(a.ReasonCodes, domain.ReasonKYCInsufficient) {
		t.Fatalf("got %+v", a)
	}
	if calls := env.Sim.BureauCalls(id); calls != 0 {
		t.Fatalf("%d bureau enquiries were made for an ineligible customer", calls)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.bureau_reports`); n != 0 {
		t.Fatalf("%d bureau reports stored", n)
	}
	// The BVN was never released by identity either.
	var released int
	if err := env.Identity.Pool.QueryRow(ctx, `SELECT count(*) FROM identity.audit_log WHERE action = 'BUREAU_SUBJECT_RELEASED'`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if released != 0 {
		t.Fatalf("BVN released %d times for an ineligible customer", released)
	}
}

// While the bureau is unavailable the application neither approves nor
// declines. It waits, visibly, and completes when the bureau returns.
func TestBureauOutageDefersTheDecisionAndRecovers(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})

	for name, behaviour := range map[string]sim.BureauBehaviour{
		"timeout":   {Fail: "timeout"},
		"http 500":  {Fail: "http500"},
		"malformed": {Fail: "malformed"},
	} {
		c := env.Identity.Register(t)
		env.Sim.SetBureauBehaviour(c.BVN, behaviour)
		id := submit(t, env, c, amount100k, 3, income300k)

		if err := env.Service.Assess(ctx, uuid.MustParse(id)); err != nil {
			t.Fatalf("%s: assess: %v", name, err)
		}
		a := getApplication(t, env, c, id)
		if a.Status != "PROCESSING" || a.Message == "" || a.Offer != nil {
			t.Fatalf("%s: during the outage got %+v; want PROCESSING with a waiting message", name, a)
		}
		var waitingOn string
		if err := env.Pool.QueryRow(ctx, `SELECT waiting_on FROM lending.applications WHERE application_id = $1`, id).Scan(&waitingOn); err != nil {
			t.Fatal(err)
		}
		if waitingOn != "CREDIT_BUREAU" {
			t.Fatalf("%s: waiting_on = %q", name, waitingOn)
		}
		if n := count(t, env, `SELECT count(*) FROM lending.decision_snapshots WHERE application_id = $1`, id); n != 0 {
			t.Fatalf("%s: a decision was recorded without a bureau report", name)
		}

		// Not due yet: an immediate sweep does nothing (backoff is respected).
		assess(t, env)
		if a := getApplication(t, env, c, id); a.Status != "PROCESSING" {
			t.Fatalf("%s: status changed before the retry was due: %+v", name, a)
		}

		// The bureau recovers; after the backoff the sweeper completes it.
		score := 720
		env.Sim.SetBureauBehaviour(c.BVN, sim.BureauBehaviour{Score: &score})
		env.Clock.Advance(env.Config.RetryBackoff[0] + time.Second)
		assess(t, env)
		if a := getApplication(t, env, c, id); a.Status != "OFFER_READY" {
			t.Fatalf("%s: after recovery got %+v", name, a)
		}
	}
}

// If the bureau stays down for the application's whole validity, the
// application expires with a truthful reason. It is never approved blind.
func TestApplicationExpiresIfBureauNeverReturns(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := registerWithBVN(t, env, "4209")
	id := submit(t, env, c, amount100k, 3, income300k)
	assess(t, env)

	env.Clock.Advance(25 * time.Hour)
	if err := env.Service.ExpireStale(ctx); err != nil {
		t.Fatal(err)
	}
	login(t, env, &c)
	a := getApplication(t, env, c, id)
	if a.Status != "EXPIRED" || !slices.Contains(a.ReasonCodes, domain.ReasonBureauUnavailable) {
		t.Fatalf("got %+v", a)
	}
	// The slot is free again: the customer can apply afresh.
	submit(t, env, c, amount100k, 3, income300k)
}

// Retrying an assessment repeats the same bureau enquiry (same reference),
// and the simulator, like a real bureau should, returns the same report.
func TestAssessmentRetriesReuseTheBureauReference(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := registerWithBVN(t, env, "4210") // fails twice, then answers
	id := submit(t, env, c, amount100k, 3, income300k)

	for i := 0; i < 3; i++ {
		assess(t, env)
		env.Clock.Advance(env.Config.RetryBackoff[min(i, len(env.Config.RetryBackoff)-1)] + time.Second)
	}
	a := getApplication(t, env, c, id)
	if a.Status != "OFFER_READY" {
		t.Fatalf("got %+v", a)
	}
	if calls := env.Sim.BureauCalls(id); calls != 3 {
		t.Fatalf("bureau received %d calls under the application's reference, want 3 (2 failures + 1 success)", calls)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.bureau_reports WHERE application_id = $1`, id); n != 1 {
		t.Fatalf("%d bureau reports stored, want 1", n)
	}
}

// A decision can be reproduced from what was stored: replaying the snapshot's
// features through the same policy and product gives the same decision.
func TestStoredDecisionIsReproducible(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	for _, suffix := range []string{"4242", "4203", "4204", "4206"} {
		c := registerWithBVN(t, env, suffix)
		id := submit(t, env, c, 30_000_000, 3, income300k)
		assess(t, env)

		snapshots, err := env.Store.Decisions(ctx, uuid.MustParse(id))
		if err != nil || len(snapshots) != 1 {
			t.Fatalf("suffix %s: snapshots %d err %v", suffix, len(snapshots), err)
		}
		s := snapshots[0]
		if s.PolicyVersion != env.Policy.Version || s.ProductVersion != env.Product.Version || s.DecidedBy != "AUTO" {
			t.Fatalf("suffix %s: snapshot versions %+v", suffix, s)
		}
		replayed := domain.Evaluate(env.Policy, env.Product, s.Features)
		if !reflect.DeepEqual(replayed, s.Decision) {
			t.Fatalf("suffix %s: replay %+v differs from stored %+v", suffix, replayed, s.Decision)
		}
		if s.InputRefs["bureau_report"] == "" {
			t.Fatalf("suffix %s: snapshot does not point at its bureau report", suffix)
		}
		// The raw bureau response is kept, with its hash.
		if n := count(t, env, `SELECT count(*) FROM lending.bureau_reports WHERE report_id = $1 AND raw_sha256 = encode(sha256(raw), 'hex')`, s.InputRefs["bureau_report"]); n != 1 {
			t.Fatalf("suffix %s: bureau report missing or hash mismatch", suffix)
		}
	}
	// Snapshots and reports are append-only for the application role.
	if _, err := env.Pool.Exec(ctx, `UPDATE lending.decision_snapshots SET outcome = 'APPROVE'`); err == nil {
		t.Fatal("decision snapshots must not be updatable")
	}
	if _, err := env.Pool.Exec(ctx, `UPDATE lending.audit_log SET action = 'x'`); err == nil {
		t.Fatal("the audit log must not be updatable")
	}
}

// Velocity: repeated applications are sent to review, then blocked.
func TestApplicationVelocityIsScreened(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)

	var last applicationView
	for i := 1; i <= 7; i++ {
		id := submit(t, env, c, amount100k, 3, income300k)
		assess(t, env)
		last = getApplication(t, env, c, id)
		switch {
		case i <= 3 && last.Status != "OFFER_READY":
			t.Fatalf("application %d: %+v, want an offer", i, last)
		case i > 3 && i <= 6 && (last.Status != "UNDER_REVIEW" || !slices.Contains(last.ReasonCodes, domain.ReasonFraudReview)):
			t.Fatalf("application %d: %+v, want fraud review", i, last)
		case i == 7 && (last.Status != "DECLINED" || !slices.Contains(last.ReasonCodes, domain.ReasonFraudRisk)):
			t.Fatalf("application %d: %+v, want a fraud decline", i, last)
		}
		// Free the slot for the next application.
		if last.Status == "OFFER_READY" || last.Status == "UNDER_REVIEW" {
			if st, body := env.Do(t, "POST", "/v1/loan-applications/"+id+"/cancel", c.Token, "", nil); st != http.StatusOK {
				t.Fatalf("cancel: %d %s", st, body)
			}
		}
	}
}

func TestManualReview(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := registerWithBVN(t, env, "4204") // thin file -> referred
	id := submit(t, env, c, 20_000_000, 3, income300k)
	assess(t, env)

	_, reviewer := env.Identity.StaffToken(t, authn.RoleCreditReviewer)
	_, viewer := env.Identity.StaffToken(t, authn.RoleOpsViewer)

	// Only a credit reviewer sees the queue or decides.
	if st, _ := env.Do(t, "GET", "/v1/ops/manual-reviews", viewer, "", nil); st != http.StatusForbidden {
		t.Fatalf("viewer listing reviews: %d", st)
	}
	if st, _ := env.Do(t, "GET", "/v1/ops/manual-reviews", c.Token, "", nil); st != http.StatusForbidden {
		t.Fatalf("customer listing reviews: %d", st)
	}
	st, body := env.Do(t, "GET", "/v1/ops/manual-reviews", reviewer, "", nil)
	if st != http.StatusOK {
		t.Fatalf("list reviews: %d %s", st, body)
	}
	queue := lendingtest.Decode[struct {
		Items []struct {
			ApplicationID string `json:"application_id"`
		} `json:"items"`
	}](t, body)
	if len(queue.Items) != 1 || queue.Items[0].ApplicationID != id {
		t.Fatalf("queue %+v", queue)
	}
	// The queue shows decision features, not raw identity data.
	if slices.Contains([]bool{contains(body, c.BVN), contains(body, c.Phone), contains(body, c.FullName)}, true) {
		t.Fatal("the review queue leaks personal data")
	}

	decide := func(payload map[string]any) (int, []byte) {
		return env.Do(t, "POST", "/v1/ops/manual-reviews/"+id+"/decision", reviewer, "", payload)
	}
	// A note is required.
	if st, body := decide(map[string]any{"approve": true, "approved_principal_minor": 10_000_000, "risk_band": "C", "note": "ok"}); st != 400 {
		t.Fatalf("short note: %d %s", st, body)
	}
	// A reviewer cannot exceed what affordability allows: income 300,000 at a
	// 40% budget leaves 120,000 a month, which at band C over three months
	// services well under the 500,000 asked for here.
	if st, body := decide(map[string]any{"approve": true, "approved_principal_minor": 50_000_000, "risk_band": "C", "note": "customer is known to us"}); st != http.StatusForbidden {
		t.Fatalf("over-limit approval: %d %s", st, body)
	}
	// Unknown band.
	if st, body := decide(map[string]any{"approve": true, "approved_principal_minor": 10_000_000, "risk_band": "Z", "note": "customer is known to us"}); st != 400 {
		t.Fatalf("unknown band: %d %s", st, body)
	}
	// Within limits: approved, priced at the chosen band, offer created.
	st, body = decide(map[string]any{"approve": true, "approved_principal_minor": 10_000_000, "risk_band": "C", "note": "payslips verified by phone"})
	if st != http.StatusOK {
		t.Fatalf("approve: %d %s", st, body)
	}
	a := getApplication(t, env, c, id)
	if a.Status != "OFFER_READY" || a.Offer == nil || a.Offer.PrincipalMinor != 10_000_000 || a.Offer.MonthlyRateBps != 800 {
		t.Fatalf("after review: %+v", a)
	}
	// Deciding again is refused: the application is no longer referred.
	if st, body := decide(map[string]any{"approve": false, "note": "second thoughts about this"}); st != http.StatusConflict {
		t.Fatalf("second decision: %d %s", st, body)
	}

	// The decision history shows the automated referral and the human decision.
	snaps, err := env.Store.Decisions(ctx, uuid.MustParse(id))
	if err != nil || len(snaps) != 2 || snaps[0].DecidedBy != "AUTO" || snaps[1].DecidedBy == "AUTO" || snaps[1].ReviewerNote == "" {
		t.Fatalf("decision history: %+v %v", snaps, err)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.audit_log WHERE entity_id = $1 AND actor_kind = 'staff' AND action = 'APPLICATION_APPROVE'`, id); n != 1 {
		t.Fatalf("staff decision not audited (%d rows)", n)
	}

	// A reviewer's decline.
	c2 := registerWithBVN(t, env, "4204")
	id2 := submit(t, env, c2, amount100k, 3, income300k)
	assess(t, env)
	if st, body := env.Do(t, "POST", "/v1/ops/manual-reviews/"+id2+"/decision", reviewer, "", map[string]any{"approve": false, "note": "income could not be verified"}); st != http.StatusOK {
		t.Fatalf("decline: %d %s", st, body)
	}
	if a := getApplication(t, env, c2, id2); a.Status != "DECLINED" || !slices.Contains(a.ReasonCodes, domain.ReasonManualDecline) {
		t.Fatalf("after decline: %+v", a)
	}
}

func contains(haystack []byte, needle string) bool {
	return needle != "" && len(haystack) > 0 && (string(haystack) != "" && indexOf(string(haystack), needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
