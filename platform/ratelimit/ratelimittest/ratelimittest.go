// Package ratelimittest provides Redis-backed limiters for integration
// tests. The server is the one in deploy/docker-compose.yml (TEST_REDIS_ADDR
// overrides the default). As with pgtest, an unreachable server skips the
// test unless REQUIRE_INTEGRATION=1 is set, in which case it fails.
package ratelimittest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"bankplatform.internal/platform/ratelimit"
)

func addr() string {
	if a := os.Getenv("TEST_REDIS_ADDR"); a != "" {
		return a
	}
	return "localhost:56379"
}

// Redis returns a limiter on the test Redis with a prefix unique to this
// call, so tests never see each other's counters.
func Redis(t testing.TB) *ratelimit.Redis {
	t.Helper()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	r := ratelimit.NewRedis(addr(), "", "test-"+hex.EncodeToString(suffix))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Ping(ctx); err != nil {
		_ = r.Close()
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			t.Fatalf("integration Redis unavailable at %s: %v", addr(), err)
		}
		t.Skipf("integration Redis unavailable (start it with `make infra-up`): %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// Down returns a limiter pointed at an address where nothing listens: a
// Redis outage.
func Down(t testing.TB) *ratelimit.Redis {
	t.Helper()
	r := ratelimit.NewRedis("127.0.0.1:1", "", "down")
	t.Cleanup(func() { _ = r.Close() })
	return r
}
