// Package ratelimit limits how often one subject may perform one action.
//
// It is abuse protection, not a correctness control. Nothing that protects
// money depends on it: duplicate applications are stopped by a unique index
// in PostgreSQL, PIN guessing by the lockout counter in PostgreSQL, double
// spending by row locks in the ledger. That is what makes it safe to keep the
// counters in Redis, where they can be lost, and to let requests through
// when Redis cannot be reached (see Guard).
package ratelimit

import (
	"context"
	"errors"
	"time"
)

// Limit allows Max actions per Window for each subject.
type Limit struct {
	// Name identifies the action ("login", "loan_application"). It is part
	// of the counter key and a metric label, so it must be a fixed string.
	Name   string
	Max    int
	Window time.Duration
}

func (l Limit) Validate() error {
	if l.Name == "" || l.Max < 1 || l.Window < time.Second {
		return errors.New("ratelimit: a limit needs a name, a positive maximum and a window of at least one second")
	}
	return nil
}

// Decision is the answer for one action.
type Decision struct {
	Allowed bool
	// RetryAfter is how long until the window resets; meaningful when the
	// action was refused.
	RetryAfter time.Duration
}

// Limiter counts an action against a subject and says whether it is allowed.
// An error means the limiter could not decide.
type Limiter interface {
	Allow(ctx context.Context, limit Limit, subject string) (Decision, error)
}

// Disabled allows everything. It is used where no Redis is configured
// (local runs and most tests).
type Disabled struct{}

func (Disabled) Allow(context.Context, Limit, string) (Decision, error) {
	return Decision{Allowed: true}, nil
}
