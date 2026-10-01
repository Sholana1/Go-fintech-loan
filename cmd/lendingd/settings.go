package main

import (
	"fmt"
	"strings"
	"time"

	"bankplatform.internal/platform/config"
	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/services/lending/providers/paystack"
)

type settings struct {
	Env                config.Environment
	DatabaseURL        string
	HTTPAddr           string
	AdminAddr          string
	TLSCert, TLSKey    string
	TLSCA              string
	LedgerTarget       string
	LedgerServerName   string
	IdentityTarget     string
	IdentityServerName string
	TokenPublicKeyFile string
	KafkaBrokers       []string
	ProductFile        string
	PolicyFile         string
	BureauProvider     string
	PayoutProvider     string
	CollectionProvider string

	// Simulator configuration: local and test environments only.
	SimURL            string
	SimAPIKey         string
	SimCallbackSecret string

	// Paystack configuration: used when a provider is "paystack".
	PaystackBaseURL   string
	PaystackSecretKey string
	PaystackWebhooks  bool

	RedisAddr         string
	RedisPassword     string
	ApplicationLimit  ratelimit.Limit
	AssessmentWorkers int
	MaxDBConns        int
	OTLPEndpoint      string
}

func loadSettings() (settings, error) {
	l := config.FromEnv()
	s := settings{
		Env:                l.Environment(),
		DatabaseURL:        l.String("LENDING_DATABASE_URL"),
		HTTPAddr:           l.StringOr("LENDING_HTTP_ADDR", ":8003"),
		AdminAddr:          l.StringOr("LENDING_ADMIN_ADDR", ":7103"),
		TLSCert:            l.String("LENDING_TLS_CERT_FILE"),
		TLSKey:             l.String("LENDING_TLS_KEY_FILE"),
		TLSCA:              l.String("TLS_CA_FILE"),
		LedgerTarget:       l.String("LEDGER_GRPC_TARGET"),
		LedgerServerName:   l.StringOr("LEDGER_TLS_SERVER_NAME", "ledger"),
		IdentityTarget:     l.String("IDENTITY_GRPC_TARGET"),
		IdentityServerName: l.StringOr("IDENTITY_TLS_SERVER_NAME", "identity"),
		TokenPublicKeyFile: l.String("TOKEN_PUBLIC_KEY_FILE"),
		KafkaBrokers:       strings.Split(l.String("KAFKA_BROKERS"), ","),
		ProductFile:        l.StringOr("LENDING_PRODUCT_FILE", ""),
		PolicyFile:         l.StringOr("LENDING_POLICY_FILE", ""),
		BureauProvider:     l.String("LENDING_BUREAU_PROVIDER"),
		PayoutProvider:     l.StringOr("LENDING_PAYOUT_PROVIDER", "disabled"),
		CollectionProvider: l.StringOr("LENDING_COLLECTION_PROVIDER", "disabled"),
		SimURL:             l.StringOr("PROVIDER_SIM_URL", ""),
		SimAPIKey:          l.StringOr("PROVIDER_SIM_API_KEY", ""),
		SimCallbackSecret:  l.StringOr("PROVIDER_SIM_CALLBACK_SECRET", ""),
		PaystackBaseURL:    l.StringOr("PAYSTACK_BASE_URL", paystack.DefaultBaseURL),
		PaystackSecretKey:  l.StringOr("PAYSTACK_SECRET_KEY", ""),
		// Off by default: the webhook signing scheme has not been verified
		// against Paystack (see package paystack). Status queries do the work.
		PaystackWebhooks: l.BoolOr("PAYSTACK_WEBHOOKS_ENABLED", false),
		// Redis is optional locally; without it submissions are not rate
		// limited (the one-open-application rule in PostgreSQL still applies).
		RedisAddr:     l.StringOr("REDIS_ADDR", ""),
		RedisPassword: l.StringOr("REDIS_PASSWORD", ""),
		ApplicationLimit: ratelimit.Limit{
			Name:   "loan_application",
			Max:    l.IntOr("LENDING_APPLICATION_RATE_MAX", 5, 1, 10_000),
			Window: l.DurationOr("LENDING_APPLICATION_RATE_WINDOW", time.Minute),
		},
		AssessmentWorkers: l.IntOr("LENDING_ASSESSMENT_WORKERS", 8, 1, 256),
		// Workers plus request handlers; well below the server's limit.
		MaxDBConns:   l.IntOr("LENDING_DB_MAX_CONNS", 30, 4, 200),
		OTLPEndpoint: l.StringOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}

	// Provider selection. Two kinds of adapter exist and they are configured
	// separately so that one can never be mistaken for the other:
	//
	//   "simulator"  talks to cmd/providersim with PROVIDER_SIM_* settings.
	//                Refused outside local and test environments.
	//   "paystack"   talks to Paystack with PAYSTACK_* settings. Written from
	//                Paystack's public OpenAPI specification; not yet run
	//                against Paystack (see docs/integrations).
	//
	// The credit bureau has no production adapter: no bureau publishes a
	// contract we could verify, so it is a launch dependency.
	usesSim, usesPaystack := false, false
	switch s.BureauProvider {
	case "simulator":
		usesSim = true
	default:
		l.Fail(`LENDING_BUREAU_PROVIDER: only "simulator" is implemented; a production credit-bureau adapter is a launch dependency`)
	}
	for _, p := range []struct{ key, value string }{
		{"LENDING_PAYOUT_PROVIDER", s.PayoutProvider},
		{"LENDING_COLLECTION_PROVIDER", s.CollectionProvider},
	} {
		switch p.value {
		case "simulator":
			usesSim = true
		case "paystack":
			usesPaystack = true
		case "disabled":
		default:
			l.Fail(p.key + `: must be "simulator", "paystack" or "disabled"`)
		}
	}
	if usesSim {
		if !s.Env.AllowsSimulators() {
			l.Fail(fmt.Sprintf("simulated providers are refused in environment %q", s.Env))
		}
		if s.SimURL == "" || s.SimAPIKey == "" {
			l.Fail("PROVIDER_SIM_URL and PROVIDER_SIM_API_KEY are required when a simulator provider is selected")
		}
		if (s.PayoutProvider == "simulator" || s.CollectionProvider == "simulator") && s.SimCallbackSecret == "" {
			l.Fail("PROVIDER_SIM_CALLBACK_SECRET is required for the payout and collection simulators")
		}
	}
	if usesPaystack && s.PaystackSecretKey == "" {
		l.Fail("PAYSTACK_SECRET_KEY is required when a provider is \"paystack\"")
	}
	if err := s.ApplicationLimit.Validate(); err != nil {
		l.Fail(err.Error())
	}
	if s.RedisAddr == "" && !s.Env.AllowsSimulators() {
		l.Fail("REDIS_ADDR is required outside local and test environments (rate limiting)")
	}
	// The embedded product and policy are illustrative. Outside local and
	// test environments reviewed documents must be supplied explicitly.
	if !s.Env.AllowsSimulators() && (s.ProductFile == "" || s.PolicyFile == "") {
		l.Fail("LENDING_PRODUCT_FILE and LENDING_POLICY_FILE are required outside local and test environments")
	}
	return s, l.Err()
}
