// Command ledgerd runs the ledger service.
//
//	ledgerd serve     run the gRPC server, outbox relay and invariant verifier
//	ledgerd migrate   apply migrations and create upcoming partitions (owner role)
//	ledgerd devseed   add the development funding account (local/test only)
//	ledgerd verify    run the invariant verifier once and exit non-zero on a breach
//
// main only wires dependencies together; it holds no business logic.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/config"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/platform/kafkax"
	"bankplatform.internal/platform/lifecycle"
	"bankplatform.internal/platform/obs"
	"bankplatform.internal/platform/outbox"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/ledger/app"
	"bankplatform.internal/services/ledger/grpcapi"
	"bankplatform.internal/services/ledger/metrics"
	"bankplatform.internal/services/ledger/migrations"
	"bankplatform.internal/services/ledger/postgres"
)

// outboxLockID makes the ledger's outbox relay single-active per database.
const outboxLockID = 7_001_001

type settings struct {
	Env          config.Environment
	DatabaseURL  string
	GRPCAddr     string
	AdminAddr    string
	TLSCert      string
	TLSKey       string
	TLSCA        string
	KafkaBrokers []string
	OTLPEndpoint string
	MaxDBConns   int
	VerifyEvery  time.Duration
}

func loadSettings() (settings, error) {
	l := config.FromEnv()
	s := settings{
		Env:          l.Environment(),
		DatabaseURL:  l.String("LEDGER_DATABASE_URL"),
		GRPCAddr:     l.StringOr("LEDGER_GRPC_ADDR", ":7001"),
		AdminAddr:    l.StringOr("LEDGER_ADMIN_ADDR", ":7101"),
		TLSCert:      l.String("LEDGER_TLS_CERT_FILE"),
		TLSKey:       l.String("LEDGER_TLS_KEY_FILE"),
		TLSCA:        l.String("TLS_CA_FILE"),
		KafkaBrokers: strings.Split(l.String("KAFKA_BROKERS"), ","),
		OTLPEndpoint: l.StringOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		// Sized from the plan: burst TPS x mean transaction time is under 10;
		// 40 leaves headroom and stays far below the server's connection cap.
		MaxDBConns:  l.IntOr("LEDGER_DB_MAX_CONNS", 40, 2, 200),
		VerifyEvery: l.DurationOr("LEDGER_VERIFY_INTERVAL", time.Minute),
	}
	return s, l.Err()
}

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(context.Background())
	case "migrate":
		err = migrate(context.Background())
	case "devseed":
		err = devseed(context.Background())
	case "verify":
		err = verifyOnce(context.Background())
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledgerd:", err)
		os.Exit(1)
	}
}

func migrate(ctx context.Context) error {
	l := config.FromEnv()
	dsn := l.String("LEDGER_OWNER_DATABASE_URL")
	if err := l.Err(); err != nil {
		return err
	}
	return migrations.Apply(ctx, dsn, time.Now())
}

func devseed(ctx context.Context) error {
	l := config.FromEnv()
	env := l.Environment()
	dsn := l.String("LEDGER_OWNER_DATABASE_URL")
	if err := l.Err(); err != nil {
		return err
	}
	if !env.AllowsSimulators() {
		return fmt.Errorf("devseed is refused in environment %q", env)
	}
	return migrations.ApplyDevSeed(ctx, dsn)
}

func verifyOnce(ctx context.Context) error {
	l := config.FromEnv()
	dsn := l.String("LEDGER_DATABASE_URL")
	if err := l.Err(); err != nil {
		return err
	}
	pool, err := pgxutil.NewPool(ctx, dsn, pgxutil.PoolOptions{ApplicationName: "ledgerd-verify", MaxConns: 2, StatementTimeout: 5 * time.Minute})
	if err != nil {
		return err
	}
	defer pool.Close()
	v, err := postgres.NewStore(pool, time.Now, nil).Verify(ctx, time.Time{})
	if err != nil {
		return err
	}
	fmt.Printf("%+v\n", v)
	if v.Total() != 0 {
		return errors.New("ledger invariants violated")
	}
	return nil
}

func serve(ctx context.Context) error {
	cfg, err := loadSettings()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	tel, err := obs.Setup(ctx, obs.Config{Service: "ledger", Version: version, Environment: string(cfg.Env), OTLPEndpoint: cfg.OTLPEndpoint})
	if err != nil {
		return err
	}
	log := tel.Logger

	pool, err := pgxutil.NewPool(ctx, cfg.DatabaseURL, pgxutil.PoolOptions{
		ApplicationName: "ledgerd", MaxConns: int32(cfg.MaxDBConns),
		// Bound how long a posting may wait or run (plan 6.6).
		StatementTimeout: 5 * time.Second, LockTimeout: 2 * time.Second, IdleInTxTimeout: 10 * time.Second,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	// The store needs the metrics hook and the metrics need the policy set,
	// so read policies with a plain store first.
	bootstrap := postgres.NewStore(pool, time.Now, nil)
	rights, err := bootstrap.LoadRights(ctx)
	if err != nil {
		return fmt.Errorf("load posting rights: %w", err)
	}
	policies, err := bootstrap.LoadJournalPolicies(ctx)
	if err != nil {
		return fmt.Errorf("load journal policies: %w", err)
	}
	known := make([]string, 0, len(policies))
	for jt := range policies {
		known = append(known, jt)
	}
	rec, err := metrics.New(tel.Meter, known)
	if err != nil {
		return err
	}
	store := postgres.NewStore(pool, time.Now, rec.TxRetried)
	svc := app.NewService(store, rights, policies, rec)

	tlsCfg, err := grpcx.ServerTLS(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	grpcSrv, health, err := grpcx.NewServer(grpcx.ServerConfig{TLS: tlsCfg, Logger: log})
	if err != nil {
		return err
	}
	ledgerv1.RegisterLedgerServiceServer(grpcSrv, grpcapi.NewServer(svc, log))
	health.SetServingStatus("bank.ledger.v1.LedgerService", 1) // SERVING

	publisher, err := kafkax.NewPublisher(cfg.KafkaBrokers, "ledgerd")
	if err != nil {
		return err
	}
	defer publisher.Close()
	relay := &outbox.Relay{
		Pool: pool, Table: "ledger.outbox", Publisher: publisher, Logger: log,
		LockID: outboxLockID, OnBatch: rec.OutboxPublished,
	}

	admin := lifecycle.AdminMux(tel.MetricsHandler, pool.Ping)

	log.Info("ledger starting", "grpc_addr", cfg.GRPCAddr, "admin_addr", cfg.AdminAddr, "journal_types", len(policies))
	runErr := lifecycle.Run(ctx, log, 20*time.Second,
		lifecycle.GRPCServer("grpc", cfg.GRPCAddr, grpcSrv, 15*time.Second),
		lifecycle.HTTPServer("admin", cfg.AdminAddr, admin, 5*time.Second),
		lifecycle.Task{Name: "outbox-relay", Run: relay.Run},
		lifecycle.Every("verifier", cfg.VerifyEvery, log, func(ctx context.Context) error {
			// Journals from the last day; balances for every account.
			v, err := store.Verify(ctx, time.Now().Add(-24*time.Hour))
			if err != nil {
				return err
			}
			rec.SetViolations(v)
			if v.Total() != 0 {
				log.ErrorContext(ctx, "LEDGER INVARIANT VIOLATION", "violations", fmt.Sprintf("%+v", v))
			}
			return nil
		}),
		lifecycle.Every("outbox-age", 5*time.Second, log, func(ctx context.Context) error {
			age, err := outbox.OldestUnpublishedAge(ctx, pool, "ledger.outbox")
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

// version is overridden at build time with -ldflags.
var version = "dev"
