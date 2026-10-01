package ratelimit

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DecisionCounter returns a function for Guard.OnDecision that counts
// decisions by limit and outcome. Alert on outcome="unavailable": the
// limiter is failing open.
func DecisionCounter(meter metric.Meter) (func(limit, outcome string), error) {
	c, err := meter.Int64Counter("ratelimit_decisions_total",
		metric.WithDescription("Rate-limit decisions by limit and outcome (allowed, limited, unavailable)."))
	if err != nil {
		return nil, err
	}
	return func(limit, outcome string) {
		c.Add(context.Background(), 1, metric.WithAttributes(attribute.String("limit", limit), attribute.String("outcome", outcome)))
	}, nil
}
