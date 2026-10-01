// Package obs sets up structured logging, tracing and metrics for a service.
//
// Responsibility: one place that wires OpenTelemetry (traces over OTLP when an
// endpoint is configured; metrics exposed for Prometheus scraping) and a JSON
// logger that stamps every record with the active trace and span IDs.
// Inputs: service name, version, environment, optional OTLP endpoint.
// Outputs: a logger, a /metrics handler, a meter, and a shutdown function.
// Failure modes: an unreachable OTLP endpoint never blocks the service; spans
// are dropped by the batch processor and the exporter logs the error.
//
// Privacy: nothing here inspects payloads. Callers must not put personal data
// (names, phone numbers, BVN, account numbers) into log attributes, span
// attributes or metric labels. Identifiers used are opaque UUIDs.
package obs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Config describes the telemetry identity of a process.
type Config struct {
	Service      string
	Version      string
	Environment  string
	OTLPEndpoint string // host:port; empty disables trace export
}

// Telemetry is what a service needs from this package.
type Telemetry struct {
	Logger         *slog.Logger
	Meter          metric.Meter
	Tracer         trace.Tracer
	MetricsHandler http.Handler
	shutdown       []func(context.Context) error
}

// Shutdown flushes exporters. Call it with a bounded context on exit.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, f := range t.shutdown {
		errs = append(errs, f(ctx))
	}
	return errors.Join(errs...)
}

// Setup builds the telemetry stack and installs the global OpenTelemetry
// providers. The globals are set deliberately: instrumentation libraries
// (otelgrpc, otelhttp) read them, and there is exactly one provider per
// process.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	// A schemaless resource merges with the SDK's default resource whatever
	// semantic-convention version the SDK ships; pinning a schema URL here
	// makes startup fail whenever the SDK is upgraded past it.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cfg.Service),
		attribute.String("service.version", cfg.Version),
		attribute.String("deployment.environment.name", cfg.Environment),
	))
	if err != nil {
		return nil, err
	}

	t := &Telemetry{}

	// Traces. Without an endpoint spans are still created (so logs carry
	// trace IDs and context propagates) but nothing is exported.
	tpOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if cfg.OTLPEndpoint != "" {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlptracegrpc.WithInsecure(), // in-cluster collector; TLS terminates at the collector
		)
		if err != nil {
			return nil, err
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.shutdown = append(t.shutdown, tp.Shutdown)
	t.Tracer = tp.Tracer(cfg.Service)

	// Metrics, scraped by Prometheus.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	promExp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(promExp))
	otel.SetMeterProvider(mp)
	t.shutdown = append(t.shutdown, mp.Shutdown)
	t.Meter = mp.Meter(cfg.Service)
	t.MetricsHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})

	t.Logger = NewLogger(cfg.Service, cfg.Environment)
	return t, nil
}

// NewLogger returns a JSON logger that adds trace_id and span_id when the
// context passed to a log call carries an active span.
func NewLogger(service, env string) *slog.Logger {
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(traceHandler{Handler: base}).With("service", service, "env", env)
}

type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(a)}
}

func (h traceHandler) WithGroup(n string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(n)}
}

// TraceParent returns the W3C traceparent of the active span, or "" if none.
// It is stored with outbox events so consumers can link their spans.
func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// ContextWithTraceParent returns a context that continues the given trace.
func ContextWithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}
