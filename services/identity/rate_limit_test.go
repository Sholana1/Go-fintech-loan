package identity_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/platform/ratelimit/ratelimittest"
	"bankplatform.internal/services/identity/identitytest"
	"bankplatform.internal/services/ledger/ledgertest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Sign-in attempts are limited per phone number in Redis. The limit is
// reached before the PIN lockout, and a limited request never reaches the
// PIN check at all.
func TestSignInIsRateLimitedPerPhone(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{
		Guard: &ratelimit.Guard{Limiter: ratelimittest.Redis(t), Logger: quiet()},
		// Four per minute: the test kit signs in once at registration,
		// leaving three attempts.
		LoginLimit: ratelimit.Limit{Name: "login", Max: 4, Window: time.Minute},
	})
	a, b := env.Register(t), env.Register(t)

	for i := 1; i <= 3; i++ {
		if st, body := env.Post(t, "/v1/sessions", map[string]string{"phone": a.Phone, "pin": "000001"}); st != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d %s", i, st, body)
		}
	}
	// The next is refused by the limiter, even with the correct PIN.
	st, body := env.Post(t, "/v1/sessions", map[string]string{"phone": a.Phone, "pin": a.PIN})
	if st != http.StatusTooManyRequests || errorCode(t, body) != "RATE_LIMITED" {
		t.Fatalf("over the limit: %d %s", st, body)
	}
	// Only the three attempts that were let through counted towards lockout.
	var failed int
	if err := env.Pool.QueryRow(t.Context(), `SELECT failed_attempts FROM identity.credentials WHERE customer_id = $1`, a.ID).Scan(&failed); err != nil || failed != 3 {
		t.Fatalf("failed attempts %d (%v), want 3", failed, err)
	}
	// Another customer is unaffected.
	if tok := env.Login(t, b.Phone, b.PIN); tok == "" {
		t.Fatal("another customer was limited")
	}
}

// Redis is down. Sign-in still works, and the protection that matters, the
// PIN lockout in PostgreSQL, still applies.
func TestSignInWorksAndLockoutHoldsWhenRedisIsDown(t *testing.T) {
	ledger := ledgertest.Start(t)
	env := identitytest.Start(t, ledger, identitytest.Options{
		Guard:      &ratelimit.Guard{Limiter: ratelimittest.Down(t), Logger: quiet()},
		LoginLimit: ratelimit.Limit{Name: "login", Max: 1, Window: time.Minute},
	})
	c := env.Register(t)

	if tok := env.Login(t, c.Phone, c.PIN); tok == "" {
		t.Fatal("sign-in failed because Redis is down")
	}
	if tok := env.Login(t, c.Phone, c.PIN); tok == "" {
		t.Fatal("second sign-in failed: the limiter did not fail open")
	}
	for i := 1; i <= 5; i++ {
		if st, body := env.Post(t, "/v1/sessions", map[string]string{"phone": c.Phone, "pin": "000001"}); st != http.StatusUnauthorized && i < 5 {
			t.Fatalf("wrong PIN %d: %d %s", i, st, body)
		}
	}
	st, body := env.Post(t, "/v1/sessions", map[string]string{"phone": c.Phone, "pin": c.PIN})
	if st != http.StatusTooManyRequests || errorCode(t, body) != "CREDENTIAL_LOCKED" {
		t.Fatalf("after five wrong PINs with Redis down: %d %s (the lockout must not depend on Redis)", st, body)
	}
}
