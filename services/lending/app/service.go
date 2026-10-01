package app

import (
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/services/lending/domain"
	"bankplatform.internal/services/lending/postgres"
)

// Dependencies are the collaborators of the service.
type Dependencies struct {
	Store    *postgres.Store
	Ledger   Ledger
	Identity Identity
	Bureau   CreditBureau
	Fraud    FraudScreener
	// Payouts may be nil: external payouts are then not offered and loans
	// are disbursed to the customer's deposit account only.
	Payouts PayoutProvider
	// Collections may be nil: repayments from outside the bank are then not
	// offered and customers repay from their deposit account only.
	Collections PaymentVerifier
	Clock       Clock
	Metrics     Metrics
	Logger      *slog.Logger
}

// Service implements the personal-loan use cases.
type Service struct {
	store    *postgres.Store
	ledger   Ledger
	identity Identity
	bureau   CreditBureau
	fraud    FraudScreener
	payouts  PayoutProvider
	collect  PaymentVerifier
	clock    Clock
	metrics  Metrics
	log      *slog.Logger

	product domain.Product
	policy  domain.Policy
	cfg     Config

	// kick carries application ids to the assessment workers. It is bounded;
	// when full the sweeper picks the application up instead.
	kick chan uuid.UUID
}

// NewService validates configuration and builds the service.
func NewService(d Dependencies, product domain.Product, policy domain.Policy, cfg Config) (*Service, error) {
	if d.Store == nil || d.Ledger == nil || d.Identity == nil || d.Bureau == nil || d.Fraud == nil || d.Clock == nil || d.Metrics == nil || d.Logger == nil {
		return nil, errors.New("lending: missing dependency")
	}
	if err := product.Validate(); err != nil {
		return nil, err
	}
	if err := policy.Validate(product); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Service{
		store: d.Store, ledger: d.Ledger, identity: d.Identity, bureau: d.Bureau, fraud: d.Fraud, payouts: d.Payouts, collect: d.Collections,
		clock: d.Clock, metrics: d.Metrics, log: d.Logger,
		product: product, policy: policy, cfg: cfg,
		kick: make(chan uuid.UUID, 1024),
	}, nil
}

// Product returns the product the service is running.
func (s *Service) Product() domain.Product { return s.product }

func (s *Service) now() time.Time { return s.clock.Now().UTC() }

func (s *Service) today() time.Time { return bizdate.Of(s.clock.Now()) }

func backoff(steps []time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(steps) {
		attempt = len(steps)
	}
	return steps[attempt-1]
}
