package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"bankplatform.internal/platform/outbox"
	"bankplatform.internal/platform/pgtest"
)

const table = "demo.outbox"

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := pgtest.New(t, "ledger_owner", "ledger_app")
	pool, err := pgxpool.New(context.Background(), db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, err = pool.Exec(context.Background(), `
		CREATE SCHEMA demo;
		CREATE TABLE demo.things (id int PRIMARY KEY);
		CREATE TABLE demo.outbox (
		  outbox_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		  event_id uuid NOT NULL UNIQUE, topic text NOT NULL, partition_key text NOT NULL,
		  event_type text NOT NULL, schema_version int NOT NULL,
		  aggregate_type text NOT NULL, aggregate_id text NOT NULL, aggregate_version bigint NOT NULL,
		  payload jsonb NOT NULL, traceparent text,
		  occurred_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz)`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

type fakePublisher struct {
	mu       sync.Mutex
	messages []outbox.Message
	fail     error
}

func (f *fakePublisher) Publish(_ context.Context, msgs []outbox.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.messages = append(f.messages, msgs...)
	return nil
}

func event(n int) outbox.Event {
	return outbox.Event{Topic: "demo.v1", PartitionKey: "k", Type: "demo.created", SchemaVersion: 1,
		AggregateType: "thing", AggregateID: "1", AggregateVersion: int64(n), Data: map[string]int{"n": n}}
}

func insert(t *testing.T, pool *pgxpool.Pool, id int, commit bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO demo.things VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Insert(ctx, tx, table, event(id)); err != nil {
		t.Fatal(err)
	}
	if commit {
		err = tx.Commit(ctx)
	} else {
		err = tx.Rollback(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func relay(pool *pgxpool.Pool, pub outbox.Publisher) *outbox.Relay {
	return &outbox.Relay{Pool: pool, Table: table, Publisher: pub, LockID: 4242, BatchSize: 10,
		Interval: 10 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// An event exists if and only if its business transaction committed.
func TestEventIsPublishedOnlyIfTheTransactionCommitted(t *testing.T) {
	pool := setup(t)
	pub := &fakePublisher{}
	insert(t, pool, 1, true)
	insert(t, pool, 2, false) // rolled back: no thing, no event
	insert(t, pool, 3, true)

	n, err := relay(pool, pub).DrainOnce(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("drained %d, err %v; want 2", n, err)
	}
	var versions []int64
	for _, m := range pub.messages {
		var env outbox.Envelope
		if err := json.Unmarshal(m.Value, &env); err != nil {
			t.Fatal(err)
		}
		if env.EventID == uuid.Nil || env.EventType != "demo.created" || m.Headers["event_id"] != env.EventID.String() {
			t.Fatalf("bad envelope %+v", env)
		}
		versions = append(versions, env.AggregateVersion)
	}
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 3 {
		t.Fatalf("published versions %v, want [1 3] in order", versions)
	}
	if n, _ := relay(pool, pub).DrainOnce(context.Background()); n != 0 {
		t.Fatalf("second drain published %d rows again", n)
	}
}

// If the broker is down nothing is lost: rows wait and go out later.
func TestBrokerOutageDelaysButNeverLosesEvents(t *testing.T) {
	pool := setup(t)
	pub := &fakePublisher{fail: errors.New("broker unavailable")}
	insert(t, pool, 1, true)

	if _, err := relay(pool, pub).DrainOnce(context.Background()); err == nil {
		t.Fatal("expected publish error")
	}
	age, err := outbox.OldestUnpublishedAge(context.Background(), pool, table)
	if err != nil || age <= 0 {
		t.Fatalf("oldest age %v err %v; the row must still be waiting", age, err)
	}

	pub.fail = nil
	if n, err := relay(pool, pub).DrainOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("drained %d err %v", n, err)
	}
	if age, _ := outbox.OldestUnpublishedAge(context.Background(), pool, table); age != 0 {
		t.Fatalf("outbox should be empty, oldest age %v", age)
	}
}

// crashAfterPublish acknowledges the publish and then kills the relay's
// transaction, simulating a process crash between "broker has it" and "row
// marked published".
type crashAfterPublish struct {
	inner *fakePublisher
	pool  *pgxpool.Pool
	armed bool
}

func (c *crashAfterPublish) Publish(ctx context.Context, msgs []outbox.Message) error {
	if err := c.inner.Publish(ctx, msgs); err != nil {
		return err
	}
	if c.armed {
		c.armed = false
		// Terminate every other backend of this database: the relay's open
		// transaction dies exactly as it would if the process were killed.
		_, _ = c.pool.Exec(context.WithoutCancel(ctx), `
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			 WHERE datname = current_database() AND pid <> pg_backend_pid() AND state = 'idle in transaction'`)
	}
	return nil
}

// Published-but-not-marked events are sent again with the same event_id:
// at-least-once delivery, which consumers make harmless by deduplicating.
func TestCrashAfterPublishRepublishesWithTheSameEventID(t *testing.T) {
	pool := setup(t)
	killer, err := pgx.Connect(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Skipf("cannot open superuser-capable connection: %v", err)
	}
	defer killer.Close(context.Background())

	inner := &fakePublisher{}
	admin := pgtestAdminPool(t, pool)
	pub := &crashAfterPublish{inner: inner, pool: admin, armed: true}
	insert(t, pool, 1, true)

	r := relay(pool, pub)
	if _, err := r.DrainOnce(context.Background()); err == nil {
		t.Fatal("expected the relay transaction to fail after the simulated crash")
	}
	n, err := r.DrainOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("recovery drain: n=%d err=%v", n, err)
	}
	if len(inner.messages) != 2 {
		t.Fatalf("published %d times, want 2 (original + redelivery)", len(inner.messages))
	}
	if inner.messages[0].Headers["event_id"] != inner.messages[1].Headers["event_id"] {
		t.Fatal("redelivery must carry the same event_id so consumers can deduplicate")
	}
}

// Only one relay per database is active at a time.
func TestOnlyOneRelayIsActive(t *testing.T) {
	pool := setup(t)
	pub := &fakePublisher{}
	for i := 1; i <= 50; i++ {
		insert(t, pool, i, true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = relay(pool, pub).Run(ctx) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pub.mu.Lock()
		n := len(pub.messages)
		pub.mu.Unlock()
		if n >= 50 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.messages) != 50 {
		t.Fatalf("published %d messages, want exactly 50 (no duplicates from competing relays)", len(pub.messages))
	}
	for i, m := range pub.messages {
		var env outbox.Envelope
		_ = json.Unmarshal(m.Value, &env)
		if env.AggregateVersion != int64(i+1) {
			t.Fatalf("message %d has version %d: order within the aggregate was not preserved", i, env.AggregateVersion)
		}
	}
}

func TestInsertValidatesInput(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	defer tx.Rollback(ctx)
	if _, err := outbox.Insert(ctx, tx, "demo.outbox; DROP TABLE demo.things", event(1)); err == nil {
		t.Fatal("an invalid table name must be rejected")
	}
	bad := event(1)
	bad.PartitionKey = ""
	if _, err := outbox.Insert(ctx, tx, table, bad); err == nil {
		t.Fatal("a missing partition key must be rejected")
	}
}

// pgtestAdminPool returns a superuser pool on the same database, used only
// to simulate crashes by terminating backends.
func pgtestAdminPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config().Copy()
	cfg.ConnConfig.User = "postgres"
	admin, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	return admin
}
