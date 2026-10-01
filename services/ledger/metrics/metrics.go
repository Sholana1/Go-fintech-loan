// Package metrics implements the ledger's operational signals on
// OpenTelemetry instruments (exposed to Prometheus).
//
// Label cardinality is bounded: journal_type is taken from the fixed policy
// set (anything else is reported as "unknown"), and reason is one of the
// stable contract reasons. No account, customer or operation identifier is
// ever used as a label.
package metrics

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bankplatform.internal/services/ledger/postgres"
)

// Recorder implements app.Metrics and the background gauges.
type Recorder struct {
	knownTypes map[string]bool

	posted   metric.Int64Counter
	rejected metric.Int64Counter
	duration metric.Float64Histogram
	retries  metric.Int64Counter
	outboxN  metric.Int64Counter

	violations atomic.Pointer[postgres.Violations]
	outboxAge  atomic.Int64 // nanoseconds
}

// New registers the ledger instruments on meter.
func New(meter metric.Meter, knownJournalTypes []string) (*Recorder, error) {
	r := &Recorder{knownTypes: map[string]bool{}}
	for _, t := range knownJournalTypes {
		r.knownTypes[t] = true
	}
	var err error
	if r.posted, err = meter.Int64Counter("ledger_journals_posted_total",
		metric.WithDescription("Journals posted. duplicate=true counts repeated requests answered from the posting reference (duplicate-operation prevention).")); err != nil {
		return nil, err
	}
	if r.rejected, err = meter.Int64Counter("ledger_postings_rejected_total",
		metric.WithDescription("Posting requests refused, by stable reason.")); err != nil {
		return nil, err
	}
	if r.duration, err = meter.Float64Histogram("ledger_posting_duration_seconds",
		metric.WithDescription("Time inside the posting transaction."),
		metric.WithExplicitBucketBoundaries(0.001, 0.0025, 0.005, 0.01, 0.02, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5)); err != nil {
		return nil, err
	}
	if r.retries, err = meter.Int64Counter("ledger_tx_retries_total",
		metric.WithDescription("Transactions retried after a serialization failure or deadlock, by SQLSTATE. Sustained growth means lock contention.")); err != nil {
		return nil, err
	}
	if r.outboxN, err = meter.Int64Counter("ledger_outbox_published_total",
		metric.WithDescription("Outbox events published to Kafka.")); err != nil {
		return nil, err
	}

	if _, err = meter.Int64ObservableGauge("ledger_invariant_violations",
		metric.WithDescription("Breaches of ledger invariants found by the verifier. Any non-zero value is a page."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			v := r.violations.Load()
			if v == nil {
				return nil
			}
			o.Observe(v.UnbalancedJournals, metric.WithAttributes(attribute.String("kind", "unbalanced_journal")))
			o.Observe(v.BalanceDrift, metric.WithAttributes(attribute.String("kind", "balance_drift")))
			o.Observe(v.RunningBalanceBreaks, metric.WithAttributes(attribute.String("kind", "running_balance_break")))
			o.Observe(v.HeldDrift, metric.WithAttributes(attribute.String("kind", "held_drift")))
			o.Observe(v.DefaultPartitionRows, metric.WithAttributes(attribute.String("kind", "default_partition_rows")))
			o.Observe(v.TrialBalanceDifference, metric.WithAttributes(attribute.String("kind", "trial_balance_difference")))
			return nil
		})); err != nil {
		return nil, err
	}
	if _, err = meter.Float64ObservableGauge("ledger_outbox_oldest_unpublished_seconds",
		metric.WithDescription("Age of the oldest event not yet published. Grows while Kafka is unreachable."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(time.Duration(r.outboxAge.Load()).Seconds())
			return nil
		})); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Recorder) journalType(t string) string {
	if r.knownTypes[t] {
		return t
	}
	return "unknown"
}

// JournalPosted implements app.Metrics.
func (r *Recorder) JournalPosted(journalType string, duplicate bool, d time.Duration) {
	attrs := metric.WithAttributes(attribute.String("journal_type", r.journalType(journalType)), attribute.Bool("duplicate", duplicate))
	r.posted.Add(context.Background(), 1, attrs)
	r.duration.Record(context.Background(), d.Seconds(), metric.WithAttributes(attribute.String("journal_type", r.journalType(journalType))))
}

// PostingRejected implements app.Metrics.
func (r *Recorder) PostingRejected(journalType, reason string) {
	r.rejected.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("journal_type", r.journalType(journalType)), attribute.String("reason", reason)))
}

// TxRetried is the pgxutil.TxRunner retry hook.
func (r *Recorder) TxRetried(code string, _ int) {
	r.retries.Add(context.Background(), 1, metric.WithAttributes(attribute.String("sqlstate", code)))
}

// OutboxPublished is the outbox relay batch hook.
func (r *Recorder) OutboxPublished(n int) { r.outboxN.Add(context.Background(), int64(n)) }

// SetViolations records the latest verifier result.
func (r *Recorder) SetViolations(v postgres.Violations) { r.violations.Store(&v) }

// SetOutboxAge records the age of the oldest unpublished event.
func (r *Recorder) SetOutboxAge(d time.Duration) { r.outboxAge.Store(int64(d)) }
