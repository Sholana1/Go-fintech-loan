// Package config reads process configuration from environment variables and
// reports every problem at once, so a misconfigured service fails at startup
// with a complete list instead of at the first request that needs a value.
//
// Responsibility: typed access to environment variables with validation.
// It holds no service-specific knowledge; each service declares its own
// settings struct next to its main package.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment names the deployment environment. Simulators and insecure
// transports are refused in Production.
type Environment string

const (
	Local      Environment = "local"
	Test       Environment = "test"
	Staging    Environment = "staging"
	Production Environment = "production"
)

// Loader accumulates validation errors while values are read.
type Loader struct {
	lookup func(string) (string, bool)
	errs   []error
}

// FromEnv returns a Loader backed by the process environment.
func FromEnv() *Loader { return &Loader{lookup: os.LookupEnv} }

// FromMap returns a Loader backed by a map; used in tests.
func FromMap(m map[string]string) *Loader {
	return &Loader{lookup: func(k string) (string, bool) { v, ok := m[k]; return v, ok }}
}

func (l *Loader) fail(key, msg string) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", key, msg))
}

// Fail records a cross-field validation error.
func (l *Loader) Fail(msg string) { l.errs = append(l.errs, errors.New(msg)) }

// Err returns all recorded problems, or nil.
func (l *Loader) Err() error { return errors.Join(l.errs...) }

// String returns a required, non-empty value.
func (l *Loader) String(key string) string {
	v, ok := l.lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		l.fail(key, "required")
		return ""
	}
	return v
}

// StringOr returns the value or a default when unset.
func (l *Loader) StringOr(key, def string) string {
	if v, ok := l.lookup(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

// IntOr returns an integer within [min, max], or the default when unset.
func (l *Loader) IntOr(key string, def, min, max int) int {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, "must be an integer")
		return def
	}
	if n < min || n > max {
		l.fail(key, fmt.Sprintf("must be between %d and %d", min, max))
		return def
	}
	return n
}

// DurationOr returns a positive duration, or the default when unset.
func (l *Loader) DurationOr(key string, def time.Duration) time.Duration {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.fail(key, "must be a positive duration such as 500ms or 5s")
		return def
	}
	return d
}

// BoolOr returns a boolean, or the default when unset.
func (l *Loader) BoolOr(key string, def bool) bool {
	v, ok := l.lookup(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, "must be true or false")
		return def
	}
	return b
}

// Environment reads APP_ENV and validates it against the known set.
func (l *Loader) Environment() Environment {
	v := Environment(l.StringOr("APP_ENV", ""))
	switch v {
	case Local, Test, Staging, Production:
		return v
	default:
		l.fail("APP_ENV", "must be one of local, test, staging, production")
		return Production // the safest interpretation if a caller ignores Err
	}
}

// AllowsSimulators reports whether simulated providers and development
// shortcuts may run in this environment. Staging is excluded on purpose: it
// should exercise real partner sandboxes.
func (e Environment) AllowsSimulators() bool { return e == Local || e == Test }
