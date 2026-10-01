package lending_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/platform/ratelimit/ratelimittest"
	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/lendingtest"
	sim "bankplatform.internal/simulator"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Application submissions are limited per customer in Redis.
func TestApplicationSubmissionIsRateLimitedPerCustomer(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{
		Guard:            &ratelimit.Guard{Limiter: ratelimittest.Redis(t), Logger: quietLog()},
		ApplicationLimit: ratelimit.Limit{Name: "loan_application", Max: 2, Window: time.Minute},
	})
	a, b := env.Identity.Register(t), env.Identity.Register(t)
	post := func(token string) (int, []byte) {
		return env.Do(t, "POST", "/v1/loan-applications", token, lendingtest.Key(), applyBody(amount100k, 3, income300k))
	}

	if st, body := post(a.Token); st != http.StatusAccepted {
		t.Fatalf("first: %d %s", st, body)
	}
	// The second is within the rate limit and is refused by the business
	// rule in PostgreSQL (one open application), not by Redis.
	if st, body := post(a.Token); st != http.StatusConflict || lendingtest.ErrorCode(t, body) != "APPLICATION_ALREADY_OPEN" {
		t.Fatalf("second: %d %s", st, body)
	}
	if st, body := post(a.Token); st != http.StatusTooManyRequests || lendingtest.ErrorCode(t, body) != "RATE_LIMITED" {
		t.Fatalf("third: %d %s", st, body)
	}
	if st, body := post(b.Token); st != http.StatusAccepted {
		t.Fatalf("another customer: %d %s", st, body)
	}
}

// With Redis down, submissions go through and the rule that matters still
// holds: one open application per customer.
func TestSubmissionRulesHoldWhenRedisIsDown(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{
		Guard:            &ratelimit.Guard{Limiter: ratelimittest.Down(t), Logger: quietLog()},
		ApplicationLimit: ratelimit.Limit{Name: "loan_application", Max: 1, Window: time.Minute},
	})
	c := env.Identity.Register(t)
	if st, body := env.Do(t, "POST", "/v1/loan-applications", c.Token, lendingtest.Key(), applyBody(amount100k, 3, income300k)); st != http.StatusAccepted {
		t.Fatalf("submission with Redis down: %d %s", st, body)
	}
	for range 3 {
		st, body := env.Do(t, "POST", "/v1/loan-applications", c.Token, lendingtest.Key(), applyBody(amount100k, 3, income300k))
		if st != http.StatusConflict || lendingtest.ErrorCode(t, body) != "APPLICATION_ALREADY_OPEN" {
			t.Fatalf("duplicate with Redis down: %d %s", st, body)
		}
	}
	if n := count(t, env, `SELECT count(*) FROM lending.applications WHERE customer_id = $1`, c.ID); n != 1 {
		t.Fatalf("%d applications", n)
	}
}

// A payout callback that carries no outcome (an adapter that does not trust
// the callback body, as the Paystack adapter does not) makes the service ask
// the provider instead of believing the callback.
func TestPayoutCallbackWithoutAnOutcomeIsResolvedByQuery(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	_, p := externalAccept(t, env, c, "0123456796") // stays pending at the provider
	drive(t, env)
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutUnknown {
		t.Fatalf("payout %s", got.State)
	}
	hint := app.PayoutCallback{Provider: "test", EventID: uuid.NewString(), Reference: p.Reference}

	// The provider still says pending: the hint changes nothing.
	if err := env.Service.HandlePayoutCallback(ctx, hint); err != nil {
		t.Fatal(err)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutUnknown {
		t.Fatalf("after a hint while pending: %s", got.State)
	}

	// The provider now says success: the same kind of hint captures it.
	env.Sim.Resolve(p.Reference, sim.StatusSuccess, "00")
	hint.EventID = uuid.NewString()
	if err := env.Service.HandlePayoutCallback(ctx, hint); err != nil {
		t.Fatal(err)
	}
	if got := payoutState(t, env, p.ID); got.State != domain.PayoutSucceeded {
		t.Fatalf("after a hint once successful: %s", got.State)
	}
	if posted, held := balance(t, env, c); posted != 0 || held != 0 {
		t.Fatalf("posted %d held %d", posted, held)
	}
}
