package lending_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/kafkatest"
	"bankplatform.internal/platform/kafkax"
	"bankplatform.internal/platform/outbox"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/lendingtest"
)

// overdueAutoDebitLoan returns a customer with an overdue single-instalment
// loan, auto-debit authorised, an empty account, and a collection that is
// not due again for hours.
func overdueAutoDebitLoan(t *testing.T, env *lendingtest.Env) (customerCode string, loanID string) {
	t.Helper()
	c := env.Identity.Register(t)
	loanID = disbursed(t, env, c, amount100k, 1, true)
	due := firstDue(t, getLoan(t, env, c, loanID))
	advanceTo(t, env, due.AddDate(0, 0, 2))
	dailyJobs(t, env)
	if err := env.Service.CollectDue(ctx); err != nil { // takes the 9,900,000 available; 500,000 still owed
		t.Fatal(err)
	}
	if posted, _ := env.Ledger.Balance(t, c.AccountCode); posted != 0 {
		t.Fatalf("balance %d after the first collection", posted)
	}
	return c.AccountCode, loanID
}

func nextCollection(t *testing.T, env *lendingtest.Env, loanID string) time.Time {
	t.Helper()
	var next time.Time
	if err := env.Pool.QueryRow(ctx, `SELECT next_collection_at FROM lending.loans WHERE loan_id = $1`, loanID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	return next
}

func journalPostedEvent(t *testing.T, eventID uuid.UUID, creditAccount string, occurredAt time.Time) []byte {
	t.Helper()
	data, _ := json.Marshal(contract.JournalPosted{
		JournalID: 1, JournalType: "DEV_FUNDING", OpType: "DEV_FUNDING", OpID: uuid.NewString(), OpStep: "POST",
		Lines: []contract.JournalPostedLine{
			{AccountCode: "SYS:DEV_FUNDING", Direction: "DEBIT", AmountMinor: 600_000, Currency: "NGN"},
			{AccountCode: creditAccount, Direction: "CREDIT", AmountMinor: 600_000, Currency: "NGN"},
		},
	})
	raw, err := json.Marshal(outbox.Envelope{EventID: eventID, EventType: contract.EventJournalPosted, SchemaVersion: 1,
		AggregateType: "journal", AggregateID: "1", AggregateVersion: 1, OccurredAt: occurredAt, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The consumer's effect and its inbox row commit together, so an event
// delivered twice (the crash-before-acknowledgement case) is applied once.
func TestLedgerEventIsAppliedOncePerEventID(t *testing.T) {
	env := lendingtest.Start(t, lendingtest.Options{})
	account, loanID := overdueAutoDebitLoan(t, env)
	scheduled := nextCollection(t, env, loanID)
	if !scheduled.After(env.Clock.Now()) {
		t.Fatalf("precondition: the next collection should be in the future, got %s", scheduled)
	}

	eventID := uuid.New()
	event := journalPostedEvent(t, eventID, account, env.Clock.Now())

	// First delivery: the loan becomes collectable now.
	if err := env.Service.HandleLedgerEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	woken := nextCollection(t, env, loanID)
	if woken.After(env.Clock.Now()) {
		t.Fatalf("collection not brought forward: %s", woken)
	}

	// Simulate what happens next in production: the collection is attempted
	// (pushing the next attempt out again), then the SAME event is
	// redelivered because the offset commit was lost.
	if err := env.Service.CollectDue(ctx); err != nil {
		t.Fatal(err)
	}
	afterAttempt := nextCollection(t, env, loanID)
	for range 5 {
		if err := env.Service.HandleLedgerEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if got := nextCollection(t, env, loanID); !got.Equal(afterAttempt) {
		t.Fatalf("a redelivered event changed the schedule from %s to %s", afterAttempt, got)
	}
	if n := count(t, env, `SELECT count(*) FROM lending.inbox WHERE event_id = $1`, eventID); n != 1 {
		t.Fatalf("%d inbox rows for one event", n)
	}
	if applied, dup := env.Metrics.Count("event:applied"), env.Metrics.Count("event:duplicate"); applied != 1 || dup != 5 {
		t.Fatalf("applied=%d duplicate=%d, want 1/5", applied, dup)
	}

	// Events that are not ours to act on are ignored without error.
	other, _ := json.Marshal(outbox.Envelope{EventID: uuid.New(), EventType: "ledger.something.else", SchemaVersion: 1, Data: json.RawMessage(`{}`)})
	if err := env.Service.HandleLedgerEvent(ctx, other); err != nil {
		t.Fatalf("unrelated event: %v", err)
	}
	// A future schema version is an error (it goes to the dead-letter topic),
	// not a guess.
	v2, _ := json.Marshal(outbox.Envelope{EventID: uuid.New(), EventType: contract.EventJournalPosted, SchemaVersion: 2, Data: json.RawMessage(`{}`)})
	if err := env.Service.HandleLedgerEvent(ctx, v2); err == nil {
		t.Fatal("an unknown schema version must be rejected")
	}
	if err := env.Service.HandleLedgerEvent(ctx, []byte(`{not json`)); err == nil {
		t.Fatal("a malformed event must be rejected")
	}
}

// End to end through real Kafka: money arrives in the ledger; the ledger's
// outbox relay publishes the journal; lending's consumer reacts; the overdue
// loan is collected.
func TestFundsArrivingTriggerCollectionThroughKafka(t *testing.T) {
	brokers := kafkatest.Brokers(t)
	kafkatest.Topics(t, brokers, contract.TopicJournals, "lending.collections.dlq")

	env := lendingtest.Start(t, lendingtest.Options{})
	account, loanID := overdueAutoDebitLoan(t, env)

	publisher, err := kafkax.NewPublisher(brokers, "ledger-test")
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	relay := &outbox.Relay{Pool: env.Ledger.Pool, Table: "ledger.outbox", Publisher: publisher, Logger: log, LockID: 1, BatchSize: 500}

	// Publish everything the ledger has produced so far, then the funding.
	for {
		n, err := relay.DrainOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	env.Ledger.Fund(t, account, 600_000)
	if n, err := relay.DrainOnce(ctx); err != nil || n != 1 {
		t.Fatalf("relay published %d, err %v", n, err)
	}

	consumeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var once sync.Once
	consumer, err := kafkax.NewConsumer(kafkax.ConsumerConfig{
		Brokers: brokers, Group: "lending-test-" + kafkatest.Suffix(t), Topics: []string{contract.TopicJournals},
		ClientID: "lending-test", DeadLetterTopic: "lending.collections.dlq", Logger: log,
	}, func(c context.Context, r kafkax.Record) error {
		if err := env.Service.HandleLedgerEvent(c, r.Value); err != nil {
			return err
		}
		// Stop once our customer's funding event has been handled.
		var envlp outbox.Envelope
		var ev contract.JournalPosted
		if json.Unmarshal(r.Value, &envlp) == nil && json.Unmarshal(envlp.Data, &ev) == nil {
			for _, l := range ev.Lines {
				if l.AccountCode == account && l.Direction == "CREDIT" && ev.JournalType == "DEV_FUNDING" {
					once.Do(cancel)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = consumer.Run(consumeCtx)

	if next := nextCollection(t, env, loanID); next.After(env.Clock.Now()) {
		t.Fatalf("the funding event did not bring the collection forward (next %s)", next)
	}
	if err := env.Service.CollectDue(ctx); err != nil {
		t.Fatal(err)
	}
	// 10,000,000 principal + 400,000 interest was owed (two days past due is
	// inside the late-fee grace period); 9,900,000 was collected earlier,
	// leaving 500,000, so 100,000 of the 600,000 that arrived remains.
	if posted, _ := env.Ledger.Balance(t, account); posted != 100_000 {
		t.Fatalf("balance %d after collection, want 100,000", posted)
	}
	var state string
	if err := env.Pool.QueryRow(ctx, `SELECT state FROM lending.loans WHERE loan_id = $1`, loanID).Scan(&state); err != nil || state != "CLOSED" {
		t.Fatalf("loan state %s err %v", state, err)
	}
	reconcileClean(t, env)
}

// Lending's own events reach Kafka through its outbox, in order, with the
// envelope fields consumers rely on and no personal data.
func TestLendingEventsArePublishedThroughTheOutbox(t *testing.T) {
	brokers := kafkatest.Brokers(t)
	kafkatest.Topics(t, brokers, app.TopicLoans, "lending.test.dlq")

	env := lendingtest.Start(t, lendingtest.Options{})
	c := env.Identity.Register(t)
	applicationID := submit(t, env, c, amount100k, 3, income300k)
	assess(t, env)

	publisher, err := kafkax.NewPublisher(brokers, "lending-test")
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	relay := &outbox.Relay{Pool: env.Pool, Table: "lending.outbox", Publisher: publisher, Logger: log, LockID: 2}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 2 {
		t.Fatalf("relay published %d, err %v", n, err)
	}
	if age, err := outbox.OldestUnpublishedAge(ctx, env.Pool, "lending.outbox"); err != nil || age != 0 {
		t.Fatalf("outbox not empty after relay: %v %v", age, err)
	}

	consumeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var got []outbox.Envelope
	var raw [][]byte
	consumer, err := kafkax.NewConsumer(kafkax.ConsumerConfig{
		Brokers: brokers, Group: "lending-events-" + kafkatest.Suffix(t), Topics: []string{app.TopicLoans},
		ClientID: "t", DeadLetterTopic: "lending.test.dlq", Logger: log,
	}, func(_ context.Context, r kafkax.Record) error {
		if string(r.Key) != applicationID {
			return nil // another test's events on the shared topic
		}
		var e outbox.Envelope
		if err := json.Unmarshal(r.Value, &e); err != nil {
			return err
		}
		got = append(got, e)
		raw = append(raw, r.Value)
		if len(got) == 2 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = consumer.Run(consumeCtx)

	if len(got) != 2 || got[0].EventType != "loan.application.submitted" || got[1].EventType != "loan.application.offered" {
		t.Fatalf("events %+v", got)
	}
	for i, e := range got {
		if e.EventID == uuid.Nil || e.SchemaVersion != 1 || e.AggregateID != applicationID || e.OccurredAt.IsZero() {
			t.Fatalf("event %d envelope incomplete: %+v", i, e)
		}
		for _, secret := range []string{c.BVN, c.Phone, c.FullName, c.PIN} {
			if contains(raw[i], secret) {
				t.Fatalf("event %d contains personal data", i)
			}
		}
	}
	if got[1].AggregateVersion <= got[0].AggregateVersion {
		t.Fatalf("aggregate versions not increasing: %d then %d", got[0].AggregateVersion, got[1].AggregateVersion)
	}
}
