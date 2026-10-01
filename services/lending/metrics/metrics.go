// Package metrics implements lending's operational signals on OpenTelemetry
// instruments, exposed to Prometheus.
//
// Every label value comes from a small fixed set (outcome, stage, kind,
// state). No customer, application, loan or reference identifier is ever a
// label, both to keep cardinality bounded and to keep personal data out of
// telemetry.
package metrics

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bankplatform.internal/services/lending/postgres"
)

// Recorder implements app.Metrics and holds the database-derived gauges.
type Recorder struct {
	decision      metric.Float64Histogram
	stage         metric.Float64Histogram
	acceptToDisb  metric.Float64Histogram
	systemTime    metric.Float64Histogram
	intents       metric.Int64Counter
	replays       metric.Int64Counter
	duplicates    metric.Int64Counter
	provider      metric.Float64Histogram
	payoutMoves   metric.Int64Counter
	externalMoves metric.Int64Counter
	callbacks     metric.Int64Counter
	exceptions    metric.Int64Counter
	events        metric.Int64Counter
	eventAge      metric.Float64Histogram
	txRetries     metric.Int64Counter
	rpcRetries    metric.Int64Counter
	outboxOut     metric.Int64Counter

	gauges    atomic.Pointer[postgres.Gauges]
	outboxAge atomic.Int64
}

var latencyBuckets = metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300)

// New registers lending's instruments on meter.
func New(meter metric.Meter) (*Recorder, error) {
	r := &Recorder{}
	var err error
	hist := func(name, desc string) metric.Float64Histogram {
		if err != nil {
			return nil
		}
		var h metric.Float64Histogram
		h, err = meter.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit("s"), latencyBuckets)
		return h
	}
	counter := func(name, desc string) metric.Int64Counter {
		if err != nil {
			return nil
		}
		var c metric.Int64Counter
		c, err = meter.Int64Counter(name, metric.WithDescription(desc))
		return c
	}

	r.decision = hist("loan_decision_seconds", "Application submitted (T0) to decision issued (T1), by outcome.")
	r.stage = hist("loan_assessment_stage_seconds", "Duration of each assessment dependency call.")
	r.acceptToDisb = hist("loan_accept_to_disbursed_seconds", "Offer accepted (T2) to ledger posted (T3).")
	r.systemTime = hist("loan_system_time_seconds", "The five-minute measure: (T1-T0) + (T3-T2). Customer thinking time is excluded.")
	r.intents = counter("loan_posting_intents_total", "Posting intents completed, by kind and outcome (posted, rejected, deferred).")
	r.replays = counter("loan_idempotent_replays_total", "API requests answered from an existing idempotency key.")
	r.duplicates = counter("loan_duplicate_postings_prevented_total", "Ledger postings that were repeats of an already-posted reference.")
	r.provider = hist("loan_provider_call_seconds", "External provider call duration, by provider, operation and outcome.")
	r.payoutMoves = counter("loan_payout_state_changes_total", "Payout state transitions, by new state.")
	r.externalMoves = counter("loan_external_payment_state_changes_total", "External (inbound) payment state transitions, by new state.")
	r.callbacks = counter("loan_payout_callbacks_total", "Provider callbacks, by outcome (applied, duplicate, unknown_reference, rejected).")
	r.exceptions = counter("loan_recon_exceptions_opened_total", "Reconciliation exceptions opened, by kind.")
	r.events = counter("loan_events_consumed_total", "Kafka events consumed, by type and outcome.")
	r.eventAge = hist("loan_event_age_seconds", "Age of a consumed event when handled (consumer freshness).")
	r.txRetries = counter("loan_tx_retries_total", "Database transactions retried after a serialization failure or deadlock.")
	r.rpcRetries = counter("loan_ledger_rpc_retries_total", "Ledger RPCs retried with the same idempotency reference.")
	r.outboxOut = counter("loan_outbox_published_total", "Outbox events published to Kafka.")
	if err != nil {
		return nil, err
	}

	gauge := func(name, desc string, read func(g *postgres.Gauges) float64) {
		if err != nil {
			return
		}
		_, err = meter.Float64ObservableGauge(name, metric.WithDescription(desc),
			metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
				if g := r.gauges.Load(); g != nil {
					o.Observe(read(g))
				}
				return nil
			}))
	}
	gauge("loan_posting_intent_oldest_pending_seconds", "Age of the oldest posting not yet applied.", func(g *postgres.Gauges) float64 { return g.OldestPendingIntentAge.Seconds() })
	gauge("loan_disbursement_pending", "Loans accepted but not yet posted to the ledger.", func(g *postgres.Gauges) float64 { return float64(g.PendingDisbursements) })
	gauge("loan_disbursement_oldest_pending_seconds", "Age of the oldest disbursement not yet posted.", func(g *postgres.Gauges) float64 { return g.OldestPendingDisbursal.Seconds() })
	gauge("loan_payout_unresolved", "Payouts whose outcome is unknown.", func(g *postgres.Gauges) float64 { return float64(g.UnknownPayouts) })
	gauge("loan_payout_oldest_unresolved_seconds", "Age of the oldest payout whose outcome is unknown.", func(g *postgres.Gauges) float64 { return g.OldestUnknownPayoutAge.Seconds() })
	gauge("loan_external_payment_confirmed_in_flight", "External payments confirmed by the provider but not yet credited and applied.", func(g *postgres.Gauges) float64 { return float64(g.ConfirmedPaymentsInFlight) })
	gauge("loan_external_payment_oldest_confirmed_wait_seconds", "How long the oldest confirmed external payment has waited to be credited and applied.", func(g *postgres.Gauges) float64 { return g.OldestConfirmedPaymentWait.Seconds() })
	gauge("loan_applications_in_assessment", "Applications submitted or being assessed.", func(g *postgres.Gauges) float64 { return float64(g.ApplicationsAssessing) })
	gauge("loan_applications_in_manual_review", "Applications waiting for a reviewer.", func(g *postgres.Gauges) float64 { return float64(g.ApplicationsReferred) })
	if err != nil {
		return nil, err
	}
	if _, err = meter.Int64ObservableGauge("loan_recon_exceptions_open", metric.WithDescription("Open reconciliation exceptions, by kind."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if g := r.gauges.Load(); g != nil {
				for kind, n := range g.OpenExceptionsByKind {
					o.Observe(int64(n), metric.WithAttributes(attribute.String("kind", kind)))
				}
			}
			return nil
		})); err != nil {
		return nil, err
	}
	if _, err = meter.Float64ObservableGauge("loan_outbox_oldest_unpublished_seconds", metric.WithDescription("Age of the oldest event not yet published."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(time.Duration(r.outboxAge.Load()).Seconds())
			return nil
		})); err != nil {
		return nil, err
	}
	return r, nil
}

var bg = context.Background()

func attrs(kv ...string) metric.MeasurementOption {
	out := make([]attribute.KeyValue, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, attribute.String(kv[i], kv[i+1]))
	}
	return metric.WithAttributes(out...)
}

func okLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}

func (r *Recorder) ApplicationDecided(outcome string, d time.Duration) {
	r.decision.Record(bg, d.Seconds(), attrs("outcome", outcome))
}
func (r *Recorder) StageDuration(stage string, d time.Duration, ok bool) {
	r.stage.Record(bg, d.Seconds(), attrs("stage", stage, "result", okLabel(ok)))
}
func (r *Recorder) LoanDisbursed(acceptToPosted, systemTime time.Duration) {
	r.acceptToDisb.Record(bg, acceptToPosted.Seconds())
	r.systemTime.Record(bg, systemTime.Seconds())
}
func (r *Recorder) IntentCompleted(kind, outcome string) {
	r.intents.Add(bg, 1, attrs("kind", kind, "outcome", outcome))
}
func (r *Recorder) IdempotentReplay(endpoint string) {
	r.replays.Add(bg, 1, attrs("endpoint", endpoint))
}
func (r *Recorder) DuplicatePostingPrevented(kind string) {
	r.duplicates.Add(bg, 1, attrs("kind", kind))
}
func (r *Recorder) ProviderCall(provider, operation, outcome string, d time.Duration) {
	r.provider.Record(bg, d.Seconds(), attrs("provider", provider, "operation", operation, "outcome", outcome))
}
func (r *Recorder) PayoutStateChanged(to string) { r.payoutMoves.Add(bg, 1, attrs("to", to)) }
func (r *Recorder) ExternalPaymentStateChanged(to string) {
	r.externalMoves.Add(bg, 1, attrs("to", to))
}
func (r *Recorder) CallbackReceived(outcome string) {
	r.callbacks.Add(bg, 1, attrs("outcome", outcome))
}
func (r *Recorder) ReconException(kind string) { r.exceptions.Add(bg, 1, attrs("kind", kind)) }
func (r *Recorder) EventConsumed(eventType, outcome string, age time.Duration) {
	r.events.Add(bg, 1, attrs("event_type", eventType, "outcome", outcome))
	r.eventAge.Record(bg, age.Seconds())
}

// TxRetried is the database transaction retry hook.
func (r *Recorder) TxRetried(code string, _ int) { r.txRetries.Add(bg, 1, attrs("sqlstate", code)) }

// LedgerRPCRetried is the ledger client's retry hook.
func (r *Recorder) LedgerRPCRetried(operation string) {
	r.rpcRetries.Add(bg, 1, attrs("operation", operation))
}

// OutboxPublished is the outbox relay batch hook.
func (r *Recorder) OutboxPublished(n int) { r.outboxOut.Add(bg, int64(n)) }

// SetGauges stores the latest database-derived gauges.
func (r *Recorder) SetGauges(g postgres.Gauges) { r.gauges.Store(&g) }

// SetOutboxAge stores the age of the oldest unpublished event.
func (r *Recorder) SetOutboxAge(d time.Duration) { r.outboxAge.Store(int64(d)) }

// Nop discards every signal; used by tests that do not assert on metrics.
type Nop struct{}

func (Nop) ApplicationDecided(string, time.Duration)           {}
func (Nop) StageDuration(string, time.Duration, bool)          {}
func (Nop) LoanDisbursed(time.Duration, time.Duration)         {}
func (Nop) IntentCompleted(string, string)                     {}
func (Nop) IdempotentReplay(string)                            {}
func (Nop) DuplicatePostingPrevented(string)                   {}
func (Nop) ProviderCall(string, string, string, time.Duration) {}
func (Nop) PayoutStateChanged(string)                          {}
func (Nop) ExternalPaymentStateChanged(string)                 {}
func (Nop) CallbackReceived(string)                            {}
func (Nop) ReconException(string)                              {}
func (Nop) EventConsumed(string, string, time.Duration)        {}
