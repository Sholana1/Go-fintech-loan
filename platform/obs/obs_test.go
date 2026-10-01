package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Setup must succeed with the SDK version in go.mod (a schema-URL conflict
// here once stopped every service from starting), expose metrics, and give
// spans a trace id that propagates through a traceparent string.
func TestSetupExposesMetricsAndPropagatesTraceContext(t *testing.T) {
	tel, err := Setup(context.Background(), Config{Service: "test", Version: "v", Environment: "test"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer tel.Shutdown(context.Background())

	counter, err := tel.Meter.Int64Counter("obs_test_events_total")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 3)

	rec := httptest.NewRecorder()
	tel.MetricsHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "obs_test_events_total") {
		t.Fatalf("metrics endpoint: %d\n%s", rec.Code, rec.Body.String())
	}

	ctx, span := tel.Tracer.Start(context.Background(), "op")
	tp := TraceParent(ctx)
	span.End()
	if !strings.Contains(tp, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q does not carry the trace id", tp)
	}
	if TraceParent(context.Background()) != "" {
		t.Fatal("no span, no traceparent")
	}
	// The trace continues on the consuming side.
	_, child := tel.Tracer.Start(ContextWithTraceParent(context.Background(), tp), "consume")
	defer child.End()
	if child.SpanContext().TraceID() != span.SpanContext().TraceID() {
		t.Fatal("trace id was not propagated")
	}
}
