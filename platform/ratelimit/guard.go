package ratelimit

import (
	"log/slog"
	"net/http"
	"strconv"

	"bankplatform.internal/platform/httpx"
)

// Outcomes reported to OnDecision.
const (
	OutcomeAllowed = "allowed"
	OutcomeLimited = "limited"
	// OutcomeUnavailable: the limiter could not decide and the request was
	// let through.
	OutcomeUnavailable = "unavailable"
)

// Guard applies a Limiter to HTTP requests and fixes the failure policy.
//
// Failure policy: FAIL OPEN. If the limiter cannot decide (Redis down or
// slow), the request proceeds, the event is logged and counted, and an alert
// fires on the counter. Refusing every login because a cache is down would
// turn a minor outage into a full one, and the controls that actually
// protect accounts and money live in PostgreSQL and still apply.
type Guard struct {
	Limiter Limiter
	Logger  *slog.Logger
	// OnDecision receives the limit name and outcome, for metrics. Optional.
	OnDecision func(limit, outcome string)
}

// Allow reports whether the request may proceed. When it returns false it
// has already written a 429 response.
func (g *Guard) Allow(w http.ResponseWriter, r *http.Request, limit Limit, subject string) bool {
	if g == nil || g.Limiter == nil {
		return true
	}
	d, err := g.Limiter.Allow(r.Context(), limit, subject)
	switch {
	case err != nil:
		g.report(limit.Name, OutcomeUnavailable)
		g.Logger.WarnContext(r.Context(), "rate limiter unavailable; request allowed", "limit", limit.Name, "error", err.Error())
		return true
	case d.Allowed:
		g.report(limit.Name, OutcomeAllowed)
		return true
	}
	g.report(limit.Name, OutcomeLimited)
	seconds := int(d.RetryAfter.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	httpx.WriteError(w, r, httpx.Errorf(http.StatusTooManyRequests, "RATE_LIMITED", "too many requests; try again later"))
	return false
}

func (g *Guard) report(limit, outcome string) {
	if g.OnDecision != nil {
		g.OnDecision(limit, outcome)
	}
}
