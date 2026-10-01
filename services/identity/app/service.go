// Package app implements identity's use cases: registering a customer with a
// verified identity, authenticating customers and staff, and answering the
// internal questions other services ask (KYC status, PIN step-up, bureau
// subject).
//
// Dependencies are small interfaces declared here and injected by main.
// Consistency boundary: identity's own database. Opening the ledger account
// is a separate, idempotent call that is completed in the background if it
// fails at registration.
package app

import (
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/identity/postgres"
	"bankplatform.internal/services/identity/secrets"
)

// Config holds identity's tunable policy.
type Config struct {
	HashParams secrets.HashParams
	Lockout    postgres.LockoutPolicy
	// BureauSubjectCallers and InternalCallers are the workload identities
	// allowed to call the respective internal RPCs.
	BureauSubjectCallers []string
	InternalCallers      []string
	// RecipientLookupCallers may resolve a phone number to a customer.
	RecipientLookupCallers []string
	// TierOnBVNMatch is the KYC tier granted for a matched BVN. The tier
	// rules themselves (limits per tier) are regulatory configuration owned
	// by compliance; see the plan's compliance matrix.
	TierOnBVNMatch int
}

type Service struct {
	store    Store
	verifier IdentityVerifier
	accounts AccountOpener
	tokens   TokenIssuer
	sealer   *secrets.Sealer
	cfg      Config
	now      func() time.Time
	log      *slog.Logger
	// dummyHash is verified when a login names an unknown account, so that
	// response time does not reveal whether the phone number is registered.
	dummyHash string
}

func NewService(store Store, verifier IdentityVerifier, accounts AccountOpener, tokens TokenIssuer, sealer *secrets.Sealer, cfg Config, now func() time.Time, log *slog.Logger) (*Service, error) {
	if cfg.Lockout.MaxFailures <= 0 || cfg.Lockout.LockFor <= 0 || cfg.TierOnBVNMatch < 1 || cfg.TierOnBVNMatch > 3 {
		return nil, errors.New("identity: lockout policy and KYC tier must be configured")
	}
	dummy, err := secrets.Hash(uuid.NewString(), cfg.HashParams)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, verifier: verifier, accounts: accounts, tokens: tokens, sealer: sealer, cfg: cfg, now: now, log: log, dummyHash: dummy}, nil
}
