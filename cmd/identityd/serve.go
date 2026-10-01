package main

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	identityv1 "bankplatform.internal/gen/bank/identity/v1"
	ledgerv1 "bankplatform.internal/gen/bank/ledger/v1"
	"bankplatform.internal/platform/authn"
	"bankplatform.internal/platform/grpcx"
	"bankplatform.internal/platform/lifecycle"
	"bankplatform.internal/platform/obs"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/platform/ratelimit"
	"bankplatform.internal/services/identity/app"
	"bankplatform.internal/services/identity/grpcapi"
	"bankplatform.internal/services/identity/httpapi"
	"bankplatform.internal/services/identity/ledgerclient"
	"bankplatform.internal/services/identity/postgres"
	"bankplatform.internal/services/identity/providers/bvnsim"
	"bankplatform.internal/services/identity/secrets"
)

func serve(ctx context.Context) error {
	cfg, err := loadSettings()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	tel, err := obs.Setup(ctx, obs.Config{Service: "identity", Version: version, Environment: string(cfg.Env), OTLPEndpoint: cfg.OTLPEndpoint})
	if err != nil {
		return err
	}
	log := tel.Logger

	pool, err := pgxutil.NewPool(ctx, cfg.DatabaseURL, pgxutil.PoolOptions{
		ApplicationName: "identityd", MaxConns: 20, StatementTimeout: 5 * time.Second, LockTimeout: 3 * time.Second, IdleInTxTimeout: 10 * time.Second,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	signingKey, err := authn.LoadPrivateKey(cfg.SigningKeyFile)
	if err != nil {
		return fmt.Errorf("token signing key: %w", err)
	}
	signer, err := authn.NewSigner(signingKey, "identity", 15*time.Minute, time.Now)
	if err != nil {
		return err
	}
	hmacKey, err := secrets.DecodeKey(cfg.BVNHMACKey)
	if err != nil {
		return fmt.Errorf("IDENTITY_BVN_HMAC_KEY: %w", err)
	}
	encKey, err := secrets.DecodeKey(cfg.BVNEncKey)
	if err != nil {
		return fmt.Errorf("IDENTITY_BVN_ENC_KEY: %w", err)
	}
	sealer, err := secrets.NewSealer(hmacKey, encKey)
	if err != nil {
		return err
	}

	clientTLS, err := grpcx.ClientTLS(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA, cfg.LedgerName)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	ledgerConn, err := grpcx.Dial(grpcx.ClientConfig{Target: cfg.LedgerAddr, TLS: clientTLS, DefaultTimeout: 3 * time.Second})
	if err != nil {
		return err
	}
	defer ledgerConn.Close()

	store := postgres.NewStore(pool)
	svc, err := app.NewService(store, bvnsim.New(cfg.SimURL, cfg.SimAPIKey, cfg.KYCTimeout), ledgerclient.New(ledgerv1.NewLedgerServiceClient(ledgerConn), log), signer, sealer, app.Config{
		HashParams:             secrets.DefaultHashParams,
		Lockout:                postgres.LockoutPolicy{MaxFailures: 5, LockFor: 15 * time.Minute},
		InternalCallers:        []string{"lending", "payments"},
		BureauSubjectCallers:   []string{"lending"},
		RecipientLookupCallers: []string{"payments"},
		TierOnBVNMatch:         cfg.KYCTier,
	}, time.Now, log)
	if err != nil {
		return err
	}

	// Sign-in rate limiting. Without Redis (local only) it is off.
	var guard *ratelimit.Guard
	if cfg.RedisAddr != "" {
		limiter := ratelimit.NewRedis(cfg.RedisAddr, cfg.RedisPassword, "identity")
		defer limiter.Close()
		count, err := ratelimit.DecisionCounter(tel.Meter)
		if err != nil {
			return err
		}
		guard = &ratelimit.Guard{Limiter: limiter, Logger: log, OnDecision: count}
	}
	api := httpapi.New(svc, httpapi.Config{Logger: log, Guard: guard, LoginLimit: cfg.LoginLimit})

	serverTLS, err := grpcx.ServerTLS(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	grpcSrv, _, err := grpcx.NewServer(grpcx.ServerConfig{TLS: serverTLS, Logger: log})
	if err != nil {
		return err
	}
	identityv1.RegisterIdentityServiceServer(grpcSrv, grpcapi.NewServer(svc, log))

	log.Info("identity starting", "http_addr", cfg.HTTPAddr, "grpc_addr", cfg.GRPCAddr, "bvn_provider", cfg.BVNProvider, "rate_limiting", guard != nil)
	runErr := lifecycle.Run(ctx, log, 20*time.Second,
		lifecycle.HTTPServer("http", cfg.HTTPAddr, otelhttp.NewHandler(api, "identity.http"), 10*time.Second),
		lifecycle.GRPCServer("grpc", cfg.GRPCAddr, grpcSrv, 10*time.Second),
		lifecycle.HTTPServer("admin", cfg.AdminAddr, lifecycle.AdminMux(tel.MetricsHandler, store.Ping), 5*time.Second),
		lifecycle.Every("complete-pending-accounts", 30*time.Second, log, svc.CompletePendingAccounts),
	)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tel.Shutdown(shutdownCtx)
	return runErr
}
