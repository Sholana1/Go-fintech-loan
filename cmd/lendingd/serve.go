package main

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	identityv1 "bankplatform.internal/gen/bank/identity/v1"
	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/authn"
	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/platform/kafkax"
	"bankplatform.internal/platform/lifecycle"
	"bankplatform.internal/platform/obs"
	"bankplatform.internal/platform/outbox"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/app"
	lendingconfig "bankplatform.internal/services/lending/config"
	"bankplatform.internal/services/lending/httpapi"
	"bankplatform.internal/services/lending/identityclient"
	"bankplatform.internal/services/lending/ledgerclient"
	"bankplatform.internal/services/lending/metrics"
	"bankplatform.internal/services/lending/postgres"
	"bankplatform.internal/services/lending/providers/fraudrules"
)

// outboxLockID makes lending's outbox relay single-active per database.
const outboxLockID = 7_003_001

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func serve(ctx context.Context) error {
	cfg, err := loadSettings()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	product, policy, err := lendingconfig.Load(cfg.ProductFile, cfg.PolicyFile)
	if err != nil {
		return err
	}

	tel, err := obs.Setup(ctx, obs.Config{Service: "lending", Version: version, Environment: string(cfg.Env), OTLPEndpoint: cfg.OTLPEndpoint})
	if err != nil {
		return err
	}
	log := tel.Logger
	rec, err := metrics.New(tel.Meter)
	if err != nil {
		return err
	}

	pool, err := pgxutil.NewPool(ctx, cfg.DatabaseURL, pgxutil.PoolOptions{
		ApplicationName: "lendingd", MaxConns: int32(cfg.MaxDBConns),
		StatementTimeout: 10 * time.Second, LockTimeout: 5 * time.Second, IdleInTxTimeout: 15 * time.Second,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	store := postgres.NewStore(pool, rec.TxRetried)

	// One client certificate identifies this workload ("lending") to both
	// the ledger and identity; each verifies it and authorises on it.
	ledgerTLS, err := grpcx.ClientTLS(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA, cfg.LedgerServerName)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	ledgerConn, err := grpcx.Dial(grpcx.ClientConfig{Target: cfg.LedgerTarget, TLS: ledgerTLS, DefaultTimeout: 5 * time.Second})
	if err != nil {
		return err
	}
	defer ledgerConn.Close()
	identityTLS, err := grpcx.ClientTLS(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA, cfg.IdentityServerName)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	identityConn, err := grpcx.Dial(grpcx.ClientConfig{Target: cfg.IdentityTarget, TLS: identityTLS, DefaultTimeout: 3 * time.Second})
	if err != nil {
		return err
	}
	defer identityConn.Close()

	appCfg := app.DefaultConfig()
	deps := app.Dependencies{
		Store:    store,
		Ledger:   ledgerclient.New(ledgerv1.NewLedgerServiceClient(ledgerConn), string(product.Currency), 2*time.Second, log, rec.LedgerRPCRetried),
		Identity: identityclient.New(identityv1.NewIdentityServiceClient(identityConn)),
		Fraud:    fraudrules.Screener{MaxApplications24h: policy.MaxApplications24h},
		Clock:    systemClock{}, Metrics: rec, Logger: log,
	}
	providers, err := buildProviders(cfg, string(product.Currency))
	if err != nil {
		return err
	}
	deps.Bureau, deps.Payouts, deps.Collections = providers.Bureau, providers.Payouts, providers.Collections
	svc, err := app.NewService(deps, product, policy, appCfg)
	if err != nil {
		return err
	}

	pub, err := authn.LoadPublicKey(cfg.TokenPublicKeyFile)
	if err != nil {
		return fmt.Errorf("token public key: %w", err)
	}
	verifier, err := authn.NewVerifier(pub, "identity", time.Now)
	if err != nil {
		return err
	}
	// Submission rate limiting. Without Redis (local only) it is off.
	var guard *ratelimit.Guard
	if cfg.RedisAddr != "" {
		limiter := ratelimit.NewRedis(cfg.RedisAddr, cfg.RedisPassword, "lending")
		defer limiter.Close()
		count, err := ratelimit.DecisionCounter(tel.Meter)
		if err != nil {
			return err
		}
		guard = &ratelimit.Guard{Limiter: limiter, Logger: log, OnDecision: count}
	}
	api := httpapi.New(svc, httpapi.Config{
		Verifier: verifier, Now: time.Now, Logger: log,
		PayoutCallbacks: providers.PayoutCallbacks, PaymentWebhooks: providers.PaymentWebhooks,
		Guard: guard, ApplicationLimit: cfg.ApplicationLimit,
		OnCallbackRejected: func() { rec.CallbackReceived("rejected") },
	})

	publisher, err := kafkax.NewPublisher(cfg.KafkaBrokers, "lendingd")
	if err != nil {
		return err
	}
	defer publisher.Close()
	relay := &outbox.Relay{Pool: pool, Table: postgres.OutboxTable, Publisher: publisher, Logger: log, LockID: outboxLockID, OnBatch: rec.OutboxPublished}

	consumer, err := kafkax.NewConsumer(kafkax.ConsumerConfig{
		Brokers: cfg.KafkaBrokers, Group: app.ConsumerCollections, Topics: []string{contract.TopicJournals},
		ClientID: "lendingd", DeadLetterTopic: "lending.collections.dlq", Logger: log,
	}, func(ctx context.Context, r kafkax.Record) error { return svc.HandleLedgerEvent(ctx, r.Value) })
	if err != nil {
		return err
	}

	log.Info("lending starting", "http_addr", cfg.HTTPAddr, "product", product.ID, "product_version", product.Version,
		"policy_version", policy.Version, "bureau_provider", cfg.BureauProvider, "payout_provider", cfg.PayoutProvider,
		"collection_provider", cfg.CollectionProvider, "rate_limiting", guard != nil)

	every := func(name string, d time.Duration, fn func(context.Context) error) lifecycle.Task {
		return lifecycle.Every(name, d, log, fn)
	}
	runErr := lifecycle.Run(ctx, log, 30*time.Second,
		// otelhttp starts a server span per request and continues an incoming
		// W3C trace, so one trace follows a request through REST, gRPC and
		// (via the outbox traceparent) the events it causes.
		lifecycle.HTTPServer("http", cfg.HTTPAddr, otelhttp.NewHandler(api, "lending.http"), 15*time.Second),
		lifecycle.HTTPServer("admin", cfg.AdminAddr, lifecycle.AdminMux(tel.MetricsHandler, pool.Ping), 5*time.Second),
		lifecycle.Task{Name: "assessment-workers", Run: func(ctx context.Context) error { return svc.RunAssessmentWorkers(ctx, cfg.AssessmentWorkers) }},
		lifecycle.Task{Name: "outbox-relay", Run: relay.Run},
		lifecycle.Task{Name: "ledger-event-consumer", Run: consumer.Run},
		// Recovery sweepers. Each claims work with a lease, so running
		// several instances of the service is safe.
		every("assessment-sweeper", 2*time.Second, svc.AssessDue),
		every("posting-sweeper", 2*time.Second, svc.ProcessDueIntents),
		every("payout-driver", time.Second, svc.DriveDuePayouts),
		every("external-payment-driver", 2*time.Second, svc.DriveDueExternalPayments),
		every("collections", 30*time.Second, svc.CollectDue),
		every("daily-jobs", time.Minute, svc.RunDailyJobs),
		every("expiry", time.Minute, svc.ExpireStale),
		every("ledger-reconciliation", 5*time.Minute, func(ctx context.Context) error {
			res, err := svc.ReconcileLedger(ctx, time.Now().Add(-24*time.Hour))
			if err == nil && !res.Skipped && (res.ControlBreaks > 0 || res.JournalBreaks > 0) {
				log.ErrorContext(ctx, "LEDGER RECONCILIATION BREAK", "control_breaks", res.ControlBreaks, "journal_breaks", res.JournalBreaks)
			}
			return err
		}),
		every("payout-reconciliation", 15*time.Minute, func(ctx context.Context) error {
			today := bizdate.Of(time.Now())
			for _, d := range []time.Time{today.AddDate(0, 0, -1), today} {
				if _, err := svc.ReconcilePayouts(ctx, d); err != nil {
					return err
				}
			}
			return nil
		}),
		every("gauges", 10*time.Second, func(ctx context.Context) error {
			g, err := store.ReadGauges(ctx, time.Now())
			if err != nil {
				return err
			}
			rec.SetGauges(g)
			age, err := outbox.OldestUnpublishedAge(ctx, pool, postgres.OutboxTable)
			if err != nil {
				return err
			}
			rec.SetOutboxAge(age)
			return nil
		}),
	)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tel.Shutdown(shutdownCtx)
	return runErr
}
