package kafkax_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"bankplatform.internal/platform/kafkatest"
	"bankplatform.internal/platform/kafkax"
	"bankplatform.internal/platform/outbox"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A record whose effect was applied but whose offset was never committed
// (the process died in between) is delivered again. The handler's own
// deduplication is what keeps the effect single.
func TestRecordIsRedeliveredWhenOffsetWasNotCommitted(t *testing.T) {
	brokers := kafkatest.Brokers(t)
	sfx := kafkatest.Suffix(t)
	topic, dlq, group := "t.events."+sfx, "t.dlq."+sfx, "t.group."+sfx
	kafkatest.Topics(t, brokers, topic, dlq)

	pub, err := kafkax.NewPublisher(brokers, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if err := pub.Publish(context.Background(), []outbox.Message{
		{Topic: topic, Key: "k", Value: []byte("payload"), Headers: map[string]string{"event_id": "e-1"}},
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	deliveries, effects := 0, 0
	seen := map[string]bool{} // stands in for the consumer's inbox table

	run := func(crashAfterEffect bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := kafkax.NewConsumer(kafkax.ConsumerConfig{
			Brokers: brokers, Group: group, Topics: []string{topic}, ClientID: "test", DeadLetterTopic: dlq, Logger: quiet(),
		}, func(ctx context.Context, r kafkax.Record) error {
			mu.Lock()
			defer mu.Unlock()
			deliveries++
			if !seen[r.Headers["event_id"]] {
				seen[r.Headers["event_id"]] = true
				effects++
			}
			if crashAfterEffect {
				cancel() // the process dies before the offset is committed
				return context.Canceled
			}
			cancel() // second run: stop once the record has been handled
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Run(ctx)
	}

	run(true)
	run(false)

	if deliveries != 2 {
		t.Fatalf("deliveries = %d, want 2 (original + redelivery)", deliveries)
	}
	if effects != 1 {
		t.Fatalf("effects = %d, want 1: deduplication must absorb the redelivery", effects)
	}
}

// A record that always fails is parked on the dead-letter topic after bounded
// retries and does not block the records behind it.
func TestPoisonRecordIsDeadLetteredAndDoesNotBlockThePartition(t *testing.T) {
	brokers := kafkatest.Brokers(t)
	sfx := kafkatest.Suffix(t)
	topic, dlq, group := "t.events."+sfx, "t.dlq."+sfx, "t.group."+sfx
	kafkatest.Topics(t, brokers, topic, dlq)

	pub, err := kafkax.NewPublisher(brokers, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if err := pub.Publish(context.Background(), []outbox.Message{
		{Topic: topic, Key: "k", Value: []byte("poison")},
		{Topic: topic, Key: "k", Value: []byte("good")},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	var mu sync.Mutex
	attempts := 0
	var handled []string
	var outcomes []string
	c, err := kafkax.NewConsumer(kafkax.ConsumerConfig{
		Brokers: brokers, Group: group, Topics: []string{topic}, ClientID: "test",
		DeadLetterTopic: dlq, MaxAttempts: 3, Logger: quiet(),
		OnResult: func(_ string, outcome string, _ time.Duration) {
			mu.Lock()
			outcomes = append(outcomes, outcome)
			mu.Unlock()
		},
	}, func(_ context.Context, r kafkax.Record) error {
		mu.Lock()
		defer mu.Unlock()
		if string(r.Value) == "poison" {
			attempts++
			return errors.New("cannot process")
		}
		handled = append(handled, string(r.Value))
		cancel()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Run(ctx)

	if attempts != 3 {
		t.Fatalf("poison record attempted %d times, want exactly 3", attempts)
	}
	if len(handled) != 1 || handled[0] != "good" {
		t.Fatalf("handled %v, want [good]", handled)
	}
	if len(outcomes) != 2 || outcomes[0] != "dead_lettered" || outcomes[1] != "ok" {
		t.Fatalf("outcomes %v, want [dead_lettered ok]", outcomes)
	}

	// The poison record is on the dead-letter topic with its diagnosis.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	var dead kafkax.Record
	dc, err := kafkax.NewConsumer(kafkax.ConsumerConfig{
		Brokers: brokers, Group: group + ".dlq", Topics: []string{dlq}, ClientID: "test", DeadLetterTopic: dlq + ".unused", Logger: quiet(),
	}, func(_ context.Context, r kafkax.Record) error {
		dead = r
		cancel2()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = dc.Run(ctx2)
	if string(dead.Value) != "poison" || dead.Headers["dlq_error"] != "cannot process" || dead.Headers["dlq_group"] != group {
		t.Fatalf("unexpected dead-letter record: %q %v", dead.Value, dead.Headers)
	}
}
