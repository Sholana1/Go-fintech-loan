package main

import (
	"fmt"
	"time"

	"bankplatform.internal/platform/config"
	"bankplatform.internal/platform/ratelimit"
)

type settings struct {
	Env             config.Environment
	DatabaseURL     string
	HTTPAddr        string
	GRPCAddr        string
	AdminAddr       string
	TLSCert, TLSKey string
	TLSCA           string
	LedgerAddr      string
	LedgerName      string
	SigningKeyFile  string
	BVNHMACKey      string
	BVNEncKey       string
	BVNProvider     string
	SimURL          string
	SimAPIKey       string
	KYCTimeout      time.Duration
	RedisAddr       string
	RedisPassword   string
	LoginLimit      ratelimit.Limit
	KYCTier         int
	OTLPEndpoint    string
}

func loadSettings() (settings, error) {
	l := config.FromEnv()
	s := settings{
		Env:            l.Environment(),
		DatabaseURL:    l.String("IDENTITY_DATABASE_URL"),
		HTTPAddr:       l.StringOr("IDENTITY_HTTP_ADDR", ":8002"),
		GRPCAddr:       l.StringOr("IDENTITY_GRPC_ADDR", ":7002"),
		AdminAddr:      l.StringOr("IDENTITY_ADMIN_ADDR", ":7102"),
		TLSCert:        l.String("IDENTITY_TLS_CERT_FILE"),
		TLSKey:         l.String("IDENTITY_TLS_KEY_FILE"),
		TLSCA:          l.String("TLS_CA_FILE"),
		LedgerAddr:     l.String("LEDGER_GRPC_TARGET"),
		LedgerName:     l.StringOr("LEDGER_TLS_SERVER_NAME", "ledger"),
		SigningKeyFile: l.String("IDENTITY_TOKEN_SIGNING_KEY_FILE"),
		BVNHMACKey:     l.String("IDENTITY_BVN_HMAC_KEY"),
		BVNEncKey:      l.String("IDENTITY_BVN_ENC_KEY"),
		BVNProvider:    l.String("IDENTITY_BVN_PROVIDER"),
		SimURL:         l.StringOr("PROVIDER_SIM_URL", ""),
		SimAPIKey:      l.StringOr("PROVIDER_SIM_API_KEY", ""),
		KYCTimeout:     l.DurationOr("IDENTITY_KYC_TIMEOUT", 10*time.Second),
		// Redis is optional locally; without it logins are not rate limited
		// (the PIN lockout in PostgreSQL still applies).
		RedisAddr:     l.StringOr("REDIS_ADDR", ""),
		RedisPassword: l.StringOr("REDIS_PASSWORD", ""),
		LoginLimit: ratelimit.Limit{
			Name:   "login",
			Max:    l.IntOr("IDENTITY_LOGIN_RATE_MAX", 10, 1, 10_000),
			Window: l.DurationOr("IDENTITY_LOGIN_RATE_WINDOW", time.Minute),
		},
		// The tier granted on a BVN match is regulatory configuration.
		KYCTier:      l.IntOr("IDENTITY_KYC_TIER_ON_BVN_MATCH", 1, 1, 3),
		OTLPEndpoint: l.StringOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}
	switch s.BVNProvider {
	case "simulator":
		if !s.Env.AllowsSimulators() {
			l.Fail(fmt.Sprintf("IDENTITY_BVN_PROVIDER=simulator is refused in environment %q", s.Env))
		}
		if s.SimURL == "" || s.SimAPIKey == "" {
			l.Fail("PROVIDER_SIM_URL and PROVIDER_SIM_API_KEY are required for the BVN simulator")
		}
	default:
		// No production adapter exists: it needs verified provider
		// documentation and sandbox access (launch dependency).
		l.Fail(`IDENTITY_BVN_PROVIDER: only "simulator" is implemented; a production adapter is a launch dependency`)
	}
	if err := s.LoginLimit.Validate(); err != nil {
		l.Fail(err.Error())
	}
	if s.RedisAddr == "" && !s.Env.AllowsSimulators() {
		l.Fail("REDIS_ADDR is required outside local and test environments (login rate limiting)")
	}
	return s, l.Err()
}
