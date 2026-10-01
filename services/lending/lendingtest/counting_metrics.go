package lendingtest

import (
	"sync"
	"time"

	"bankplatform.internal/services/lending/metrics"
)

// CountingMetrics counts signals by name so tests can assert on them.
type CountingMetrics struct {
	metrics.Nop
	mu     sync.Mutex
	counts map[string]int
}

func (c *CountingMetrics) inc(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[key]++
}

// Count returns how many times a signal was recorded.
func (c *CountingMetrics) Count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}

func (c *CountingMetrics) IdempotentReplay(endpoint string) { c.inc("replay:" + endpoint) }

func (c *CountingMetrics) DuplicatePostingPrevented(kind string) { c.inc("duplicate:" + kind) }

func (c *CountingMetrics) IntentCompleted(kind, outcome string) {
	c.inc("intent:" + kind + ":" + outcome)
}

func (c *CountingMetrics) PayoutStateChanged(to string) { c.inc("payout:" + to) }

func (c *CountingMetrics) ExternalPaymentStateChanged(to string) { c.inc("external_payment:" + to) }

func (c *CountingMetrics) CallbackReceived(outcome string) { c.inc("callback:" + outcome) }

func (c *CountingMetrics) ReconException(kind string) { c.inc("exception:" + kind) }

func (c *CountingMetrics) ApplicationDecided(outcome string, _ time.Duration) {
	c.inc("decided:" + outcome)
}

func (c *CountingMetrics) LoanDisbursed(_, _ time.Duration) { c.inc("disbursed") }

func (c *CountingMetrics) EventConsumed(eventType, outcome string, _ time.Duration) {
	c.inc("event:" + outcome)
}
