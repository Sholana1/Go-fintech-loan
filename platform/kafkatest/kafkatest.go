// Package kafkatest gives integration tests access to the local Kafka broker
// and creates uniquely named topics so tests do not interfere.
package kafkatest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"bankplatform.internal/platform/kafkax"
)

// Brokers returns the test broker list, skipping (or failing, under
// REQUIRE_INTEGRATION=1) when the broker is unreachable.
func Brokers(t testing.TB) []string {
	t.Helper()
	v := os.Getenv("TEST_KAFKA_BROKERS")
	if v == "" {
		v = "localhost:59092"
	}
	brokers := strings.Split(v, ",")
	p, err := kafkax.NewPublisher(brokers, "kafkatest")
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = p.Ping(ctx)
		cancel()
		p.Close()
	}
	if err != nil {
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			t.Fatalf("Kafka unavailable at %s: %v", v, err)
		}
		t.Skipf("Kafka unavailable (start it with `make infra-up`): %v", err)
	}
	return brokers
}

// Suffix returns a random suffix for topic and group names.
func Suffix(t testing.TB) string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Topics creates single-partition topics.
func Topics(t testing.TB, brokers []string, topics ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := kafkax.EnsureTopics(ctx, brokers, 1, 1, topics...); err != nil {
		t.Fatalf("create topics: %v", err)
	}
}
