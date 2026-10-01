// Package fraudrules is the fraud screen used by the personal-loan product:
// a small set of deterministic rules over the customer's own application
// behaviour.
//
// It is an in-process implementation of app.FraudScreener, not a simulator:
// these rules are real and run in every environment. The architecture plan
// places richer fraud scoring (device reputation, shared-attribute links) in
// a separate risk service; when that exists it will implement the same port
// behind gRPC and this package becomes its fallback (plan section 8.12:
// "fraud scoring down -> static rules").
//
// It uses no device content, contacts, location or messages.
package fraudrules

import (
	"context"

	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/domain"
)

// Screener applies velocity rules.
type Screener struct {
	// MaxApplications24h is the number of applications in 24 hours above
	// which an application is sent to review; at twice that it is blocked.
	MaxApplications24h int
}

// Signals reported by the screen.
const (
	SignalVelocityReview = "VELOCITY_REVIEW"
	SignalVelocityBlock  = "VELOCITY_BLOCK"
)

func (s Screener) Screen(ctx context.Context, in app.FraudInput) (app.FraudResult, error) {
	if err := ctx.Err(); err != nil {
		return app.FraudResult{}, err
	}
	res := app.FraudResult{Decision: domain.FraudClear, Signals: []string{}}
	switch {
	case in.Applications24h > 2*s.MaxApplications24h:
		res.Decision = domain.FraudBlock
		res.Signals = append(res.Signals, SignalVelocityBlock)
	case in.Applications24h > s.MaxApplications24h:
		res.Decision = domain.FraudReview
		res.Signals = append(res.Signals, SignalVelocityReview)
	}
	return res, nil
}

var _ app.FraudScreener = Screener{}
