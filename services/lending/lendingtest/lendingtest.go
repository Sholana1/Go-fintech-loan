// Package lendingtest assembles the whole personal-loan system for tests:
// real PostgreSQL databases for ledger, identity and lending; the real
// ledger and identity gRPC servers behind mutual TLS; the provider simulator
// over HTTP; and lending's REST handler on an httptest server. Nothing is
// mocked except time and, where a test asks for it, faults injected in front
// of the ledger client.
package lendingtest

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	identityv1 "bankplatform.internal/gen/bank/identity/v1"
	"bankplatform.internal/platform/pgtest"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/services/identity/identitytest"
	"bankplatform.internal/services/ledger/ledgertest"
	"bankplatform.internal/services/lending/app"
	"bankplatform.internal/services/lending/config"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/httpapi"
	"bankplatform.internal/services/lending/identityclient"
	"bankplatform.internal/services/lending/ledgerclient"
	"bankplatform.internal/services/lending/migrations"
	"bankplatform.internal/services/lending/postgres"
	"bankplatform.internal/services/lending/providers/bureausim"
	"bankplatform.internal/services/lending/providers/collectsim"
	"bankplatform.internal/services/lending/providers/fraudrules"
	"bankplatform.internal/services/lending/providers/payoutsim"
	sim "bankplatform.internal/simulator"
)

const (
	apiKey         = "sim-api-key"
	callbackSecret = "sim-callback-secret"
)

// Env is the assembled system.
type Env struct {
	Ledger   *ledgertest.Env
	Identity *identitytest.Env
	DB       pgtest.Database
	Pool     *pgxpool.Pool
	Store    *postgres.Store
	Service  *app.Service
	URL      string
	Sim      *sim.Server
	Clock    *identitytest.Clock
	Faults   *FaultyLedger
	Metrics  *CountingMetrics
	Product  domain.Product
	Policy   domain.Policy
	Config   app.Config
}

// Options customise the environment.
type Options struct {
	// Tune adjusts lending's configuration after test defaults are applied.
	Tune func(*app.Config)
	// Product adjusts the product configuration.
	Product func(*domain.Product)
	// KYCTier is the tier identity grants on registration (default 2).
	KYCTier int
	// NoPayouts runs lending without a payout provider.
	NoPayouts bool
	// NoCollections runs lending without an inbound-payment provider.
	NoCollections bool
	// Guard and ApplicationLimit enable submission rate limiting.
	Guard            *ratelimit.Guard
	ApplicationLimit ratelimit.Limit
}

// Start builds the system.
func Start(t testing.TB, opts Options) *Env {
	t.Helper()
	ctx := context.Background()
	clock := &identitytest.Clock{}

	ledger := ledgertest.StartWithClock(t, clock.Now)
	identity := identitytest.StartWithClock(t, ledger, identitytest.Options{TierOnBVNMatch: opts.KYCTier}, clock)

	db := pgtest.New(t, "lending_owner", "lending_app")
	if err := migrations.Apply(ctx, db.OwnerDSN); err != nil {
		t.Fatalf("migrate lending: %v", err)
	}
	pool, err := pgxutil.NewPool(ctx, db.AppDSN, pgxutil.PoolOptions{ApplicationName: "lending-test", MaxConns: 30, StatementTimeout: 15 * time.Second, LockTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	product, policy, err := config.Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if opts.Product != nil {
		opts.Product(&product)
	}

	simulator := sim.New(sim.Options{APIKey: apiKey, CallbackSecret: callbackSecret, Now: clock.Now})
	simSrv := httptest.NewServer(simulator.Handler())
	t.Cleanup(simSrv.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	counting := &CountingMetrics{counts: map[string]int{}}
	faults := &FaultyLedger{inner: ledgerclient.New(ledger.Client(t, "lending"), string(product.Currency), 3*time.Second, log, nil)}

	cfg := app.DefaultConfig()
	// Short deadlines so that simulated provider timeouts are quick.
	cfg.IdentityTimeout, cfg.FraudTimeout, cfg.BureauTimeout = 3*time.Second, 2*time.Second, 400*time.Millisecond
	cfg.LedgerTimeout, cfg.PayoutTimeout = 5*time.Second, 400*time.Millisecond
	cfg.AssessmentLease, cfg.PayoutLease = 10*time.Second, 10*time.Second
	cfg.CollectionTimeout, cfg.ExternalPaymentLease = 400*time.Millisecond, 15*time.Second
	if opts.Tune != nil {
		opts.Tune(&cfg)
	}

	deps := app.Dependencies{
		Store:    postgres.NewStore(pool, nil),
		Ledger:   faults,
		Identity: identityclient.New(identityv1.NewIdentityServiceClient(identity.Conn(t, "lending"))),
		Bureau:   bureausim.New(simSrv.URL, apiKey, clock.Now),
		Fraud:    fraudrules.Screener{MaxApplications24h: policy.MaxApplications24h},
		Clock:    clock, Metrics: counting, Logger: log,
	}
	if !opts.NoPayouts {
		deps.Payouts = payoutsim.New(simSrv.URL, apiKey, string(product.Currency))
	}
	if !opts.NoCollections {
		deps.Collections = collectsim.New(simSrv.URL, apiKey)
	}
	svc, err := app.NewService(deps, product, policy, cfg)
	if err != nil {
		t.Fatal(err)
	}

	api := httptest.NewServer(httpapi.New(svc, httpapi.Config{
		Verifier: identity.Verifier, Now: clock.Now, Logger: log,
		PayoutCallbacks: payoutsim.Callbacks{Secret: callbackSecret}, PaymentWebhooks: collectsim.Webhooks{Secret: callbackSecret},
		Guard: opts.Guard, ApplicationLimit: opts.ApplicationLimit,
		OnCallbackRejected: func() { counting.CallbackReceived("rejected") },
	}))
	t.Cleanup(api.Close)
	simulator.SetCallbackURL(api.URL + "/v1/provider-webhooks/payouts")
	simulator.SetCollectionWebhookURL(api.URL + "/v1/provider-webhooks/payments")

	return &Env{
		Ledger: ledger, Identity: identity, DB: db, Pool: pool, Store: deps.Store, Service: svc, URL: api.URL,
		Sim: simulator, Clock: clock, Faults: faults, Metrics: counting, Product: product, Policy: policy, Config: cfg,
	}
}
