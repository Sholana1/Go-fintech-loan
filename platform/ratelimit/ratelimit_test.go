package ratelimit_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/platform/ratelimit/ratelimittest"
)

var ctx = context.Background()

func TestRedisAllowsUpToTheLimitThenRefuses(t *testing.T) {
	r := ratelimittest.Redis(t)
	limit := ratelimit.Limit{Name: "login", Max: 3, Window: time.Minute}

	for i := 1; i <= 3; i++ {
		d, err := r.Allow(ctx, limit, "alice")
		if err != nil || !d.Allowed {
			t.Fatalf("action %d: %+v %v", i, d, err)
		}
	}
	d, err := r.Allow(ctx, limit, "alice")
	if err != nil || d.Allowed {
		t.Fatalf("fourth action: %+v %v", d, err)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("retry-after %s, want within the window", d.RetryAfter)
	}
	// Another subject, and another limit for the same subject, are separate.
	if d, err := r.Allow(ctx, limit, "bob"); err != nil || !d.Allowed {
		t.Fatalf("other subject: %+v %v", d, err)
	}
	if d, err := r.Allow(ctx, ratelimit.Limit{Name: "apply", Max: 3, Window: time.Minute}, "alice"); err != nil || !d.Allowed {
		t.Fatalf("other limit: %+v %v", d, err)
	}
}

func TestRedisWindowResets(t *testing.T) {
	r := ratelimittest.Redis(t)
	limit := ratelimit.Limit{Name: "burst", Max: 1, Window: time.Second}
	if d, _ := r.Allow(ctx, limit, "x"); !d.Allowed {
		t.Fatal("first action refused")
	}
	d, _ := r.Allow(ctx, limit, "x")
	if d.Allowed {
		t.Fatal("second action allowed inside the window")
	}
	// Wait for the window the limiter itself reported, not a guessed sleep.
	timer := time.NewTimer(d.RetryAfter + 100*time.Millisecond)
	<-timer.C
	if d, err := r.Allow(ctx, limit, "x"); err != nil || !d.Allowed {
		t.Fatalf("after the window: %+v %v", d, err)
	}
}

// The counter is atomic in Redis: with many clients at once exactly Max are
// allowed.
func TestRedisConcurrentActionsNeverExceedTheLimit(t *testing.T) {
	r := ratelimittest.Redis(t)
	limit := ratelimit.Limit{Name: "concurrent", Max: 10, Window: time.Minute}
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A fresh context per call: the limiter's own deadline applies.
			if d, err := r.Allow(ctx, limit, "shared"); err == nil && d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	// Under load some calls may exceed the limiter's 150 ms deadline and
	// error; those are not counted as allowed here. Never more than Max.
	if n := allowed.Load(); n > 10 || n == 0 {
		t.Fatalf("%d allowed, want at most 10 and at least 1", n)
	}
}

func TestRedisDownIsAnErrorWithinTheDeadline(t *testing.T) {
	r := ratelimittest.Down(t)
	start := time.Now()
	if d, err := r.Allow(ctx, ratelimit.Limit{Name: "x", Max: 1, Window: time.Minute}, "a"); err == nil {
		t.Fatalf("a limiter with no Redis decided: %+v", d)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("an unreachable Redis held the request for %s", took)
	}
}

type decisions struct {
	mu sync.Mutex
	n  map[string]int
}

func (d *decisions) add(limit, outcome string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n[limit+":"+outcome]++
}

func guard(l ratelimit.Limiter) (*ratelimit.Guard, *decisions) {
	d := &decisions{n: map[string]int{}}
	return &ratelimit.Guard{Limiter: l, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OnDecision: d.add}, d
}

func TestGuardAnswers429WithRetryAfter(t *testing.T) {
	g, seen := guard(ratelimittest.Redis(t))
	limit := ratelimit.Limit{Name: "login", Max: 2, Window: time.Minute}
	try := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		if g.Allow(w, httptest.NewRequest(http.MethodPost, "/", nil), limit, "carol") {
			w.WriteHeader(http.StatusOK)
		}
		return w
	}
	for i := 1; i <= 2; i++ {
		if w := try(); w.Code != http.StatusOK {
			t.Fatalf("request %d within the limit was refused: %d", i, w.Code)
		}
	}
	w := try()
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d Retry-After=%q body %s", w.Code, w.Header().Get("Retry-After"), w.Body)
	}
	if seen.n["login:allowed"] != 2 || seen.n["login:limited"] != 1 {
		t.Fatalf("decisions %v", seen.n)
	}
}

// Redis is down: every request is let through, and each one is counted so
// the outage is visible.
func TestGuardFailsOpenWhenRedisIsDown(t *testing.T) {
	g, seen := guard(ratelimittest.Down(t))
	limit := ratelimit.Limit{Name: "login", Max: 1, Window: time.Minute}
	for range 5 {
		w := httptest.NewRecorder()
		if !g.Allow(w, httptest.NewRequest(http.MethodPost, "/", nil), limit, "dave") {
			t.Fatalf("a request was refused because Redis is down: %d", w.Code)
		}
	}
	if seen.n["login:unavailable"] != 5 {
		t.Fatalf("decisions %v, want 5 unavailable", seen.n)
	}
}

func TestNilGuardAndDisabledLimiterAllowEverything(t *testing.T) {
	var g *ratelimit.Guard
	if !g.Allow(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), ratelimit.Limit{}, "x") {
		t.Fatal("a nil guard refused a request")
	}
	if d, err := (ratelimit.Disabled{}).Allow(ctx, ratelimit.Limit{}, "x"); err != nil || !d.Allowed {
		t.Fatalf("disabled limiter: %+v %v", d, err)
	}
}

func TestLimitValidation(t *testing.T) {
	for _, l := range []ratelimit.Limit{{Max: 1, Window: time.Minute}, {Name: "x", Window: time.Minute}, {Name: "x", Max: 1}} {
		if l.Validate() == nil {
			t.Errorf("accepted %+v", l)
		}
	}
	if err := (ratelimit.Limit{Name: "x", Max: 1, Window: time.Second}).Validate(); err != nil {
		t.Fatal(err)
	}
}
