// Package kafkax wraps the Kafka client for the two things services do with
// Kafka: publish outbox batches durably, and consume a topic at-least-once
// with bounded retries and a dead-letter topic.
//
// Kafka's role in the platform is the durable log of committed domain facts
// for many independent consumers. It is not a work queue (that is SQS, when a
// workload needs one) and it is never a source of truth for money.
//
// Exactly-once is not claimed. Producing is idempotent within a producer
// session; consumption is at-least-once; the effect is made idempotent by the
// consumer's inbox table in its own database.
package kafkax

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"bankplatform.internal/platform/outbox"
)

// Publisher publishes with acks=all and the idempotent producer.
type Publisher struct {
	client *kgo.Client
}

// NewPublisher connects a producer. clientID identifies the service.
func NewPublisher(brokers []string, clientID string) (*Publisher, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(clientID),
		kgo.RequiredAcks(kgo.AllISRAcks()), // durable on all in-sync replicas
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
		kgo.RecordDeliveryTimeout(15*time.Second),
	)
	if err != nil {
		return nil, err
	}
	return &Publisher{client: cl}, nil
}

// Publish implements outbox.Publisher.
func (p *Publisher) Publish(ctx context.Context, msgs []outbox.Message) error {
	recs := make([]*kgo.Record, 0, len(msgs))
	for _, m := range msgs {
		r := &kgo.Record{Topic: m.Topic, Key: []byte(m.Key), Value: m.Value}
		for k, v := range m.Headers {
			if v != "" {
				r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
			}
		}
		recs = append(recs, r)
	}
	return p.client.ProduceSync(ctx, recs...).FirstErr()
}

// Ping checks broker reachability; used by readiness probes.
func (p *Publisher) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

func (p *Publisher) Close() { p.client.Close() }

// Record is a consumed message.
type Record struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
	Headers   map[string]string
	Timestamp time.Time
}

// Handler processes one record. It must be idempotent: the same record can
// be delivered again after a crash or rebalance. Returning an error causes a
// bounded retry and then dead-lettering.
type Handler func(ctx context.Context, r Record) error

// ConsumerConfig configures a consumer group member.
type ConsumerConfig struct {
	Brokers  []string
	Group    string
	Topics   []string
	ClientID string
	// DeadLetterTopic receives records that still fail after MaxAttempts.
	// Dead-lettering keeps one poison record from blocking a partition.
	DeadLetterTopic string
	MaxAttempts     int // default 5
	Logger          *slog.Logger
	// OnResult is called once per record with the final outcome; for metrics.
	OnResult func(topic string, outcome string, age time.Duration)
}

// Consumer consumes records sequentially and commits offsets only after the
// handler has succeeded (or the record was dead-lettered).
type Consumer struct {
	cfg     ConsumerConfig
	client  *kgo.Client
	handler Handler
}

func NewConsumer(cfg ConsumerConfig, h Handler) (*Consumer, error) {
	if cfg.Group == "" || len(cfg.Topics) == 0 || cfg.DeadLetterTopic == "" {
		return nil, errors.New("kafkax: group, topics and dead-letter topic are required")
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID),
		kgo.ConsumerGroup(cfg.Group),
		kgo.ConsumeTopics(cfg.Topics...),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// Offsets are committed explicitly after processing. Auto-commit
		// could acknowledge a record whose database effect never committed.
		kgo.DisableAutoCommit(),
		kgo.RequiredAcks(kgo.AllISRAcks()), // for dead-letter produces
		kgo.FetchMaxBytes(5<<20),
	)
	if err != nil {
		return nil, err
	}
	return &Consumer{cfg: cfg, client: cl, handler: h}, nil
}

// Run consumes until ctx is cancelled. Records are handled one at a time,
// which bounds concurrency at one in-flight handler per consumer and
// preserves per-partition order.
func (c *Consumer) Run(ctx context.Context) error {
	defer c.client.Close()
	for {
		fetches := c.client.PollRecords(ctx, 100)
		if ctx.Err() != nil {
			return nil
		}
		if fetches.IsClientClosed() {
			return nil
		}
		for _, fe := range fetches.Errors() {
			c.cfg.Logger.ErrorContext(ctx, "kafka fetch error", "topic", fe.Topic, "partition", fe.Partition, "error", fe.Err.Error())
		}

		var processed []*kgo.Record
		var fatal error
		fetches.EachRecord(func(kr *kgo.Record) {
			if fatal != nil || ctx.Err() != nil {
				return
			}
			if err := c.handle(ctx, kr); err != nil {
				fatal = err
				return
			}
			processed = append(processed, kr)
		})

		if len(processed) > 0 {
			// If this commit fails or the process dies first, the records
			// are redelivered and the handler's inbox makes that harmless.
			commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			err := c.client.CommitRecords(commitCtx, processed...)
			cancel()
			if err != nil {
				c.cfg.Logger.ErrorContext(ctx, "kafka offset commit failed; records will be redelivered", "error", err.Error())
			}
		}
		if fatal != nil {
			// The record could be neither processed nor dead-lettered.
			// Stop without committing it so it is retried after restart.
			return fatal
		}
	}
}

func (c *Consumer) handle(ctx context.Context, kr *kgo.Record) error {
	rec := Record{
		Topic: kr.Topic, Partition: kr.Partition, Offset: kr.Offset,
		Key: kr.Key, Value: kr.Value, Timestamp: kr.Timestamp,
		Headers: make(map[string]string, len(kr.Headers)),
	}
	for _, h := range kr.Headers {
		rec.Headers[h.Key] = string(h.Value)
	}

	var err error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		if err = c.handler(ctx, rec); err == nil {
			c.result(rec, "ok")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.cfg.Logger.WarnContext(ctx, "kafka handler failed",
			"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset,
			"attempt", attempt, "max_attempts", c.cfg.MaxAttempts, "error", err.Error())
		backoff := time.Duration(100*(1<<(attempt-1))) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}

	// Poison record: park it with its diagnosis and move on.
	dlq := &kgo.Record{
		Topic: c.cfg.DeadLetterTopic, Key: kr.Key, Value: kr.Value,
		Headers: append(append([]kgo.RecordHeader{}, kr.Headers...),
			kgo.RecordHeader{Key: "dlq_error", Value: []byte(truncate(err.Error(), 500))},
			kgo.RecordHeader{Key: "dlq_source", Value: []byte(fmt.Sprintf("%s/%d/%d", kr.Topic, kr.Partition, kr.Offset))},
			kgo.RecordHeader{Key: "dlq_group", Value: []byte(c.cfg.Group)},
		),
	}
	if perr := c.client.ProduceSync(ctx, dlq).FirstErr(); perr != nil {
		return fmt.Errorf("dead-letter record %s/%d/%d: %w (handler error: %v)", kr.Topic, kr.Partition, kr.Offset, perr, err)
	}
	c.cfg.Logger.ErrorContext(ctx, "kafka record dead-lettered",
		"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "error", err.Error())
	c.result(rec, "dead_lettered")
	return nil
}

func (c *Consumer) result(rec Record, outcome string) {
	if c.cfg.OnResult != nil {
		c.cfg.OnResult(rec.Topic, outcome, time.Since(rec.Timestamp))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// EnsureTopics creates topics that do not exist. It is for local development
// and tests; production topics are created by infrastructure as code with
// the replication settings from the architecture plan.
func EnsureTopics(ctx context.Context, brokers []string, partitions int32, replication int16, topics ...string) error {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	resp, err := adm.CreateTopics(ctx, partitions, replication, nil, topics...)
	if err != nil {
		return err
	}
	for _, r := range resp {
		if r.Err != nil && !errors.Is(r.Err, kerrTopicExists) {
			return fmt.Errorf("create topic %s: %w", r.Topic, r.Err)
		}
	}
	return nil
}
