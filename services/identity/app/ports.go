package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/services/identity/domain"
	"bankplatform.internal/services/identity/postgres"
)

// ErrProviderUnavailable means the identity provider could not be reached or
// answered ambiguously. Registration is refused (fail closed): a customer is
// never created on an unverified identity.
var ErrProviderUnavailable = errors.New("identity provider unavailable")

// VerificationOutcome is the identity provider's answer.
type VerificationOutcome string

const (
	OutcomeMatch    VerificationOutcome = "MATCH"
	OutcomeMismatch VerificationOutcome = "MISMATCH"
	OutcomeNotFound VerificationOutcome = "NOT_FOUND"
)

// Verification is the result of a BVN check.
type Verification struct {
	Provider    string
	ProviderRef string
	Outcome     VerificationOutcome
}

// IdentityVerifier checks a BVN against the customer's stated details.
// ref is our reference for the check, generated before the call so the call
// can be traced and repeated safely.
type IdentityVerifier interface {
	VerifyBVN(ctx context.Context, ref, bvn, fullName string, dateOfBirth time.Time) (Verification, error)
}

// AccountOpener opens the customer's deposit account in the ledger and
// returns its code. It must be idempotent per customer.
type AccountOpener interface {
	OpenCustomerDeposit(ctx context.Context, customerID uuid.UUID) (string, error)
}

// TokenIssuer signs access tokens.
type TokenIssuer interface {
	Sign(subject string, kind authn.Kind, roles []string) (string, time.Time, error)
}

// Store is the persistence the service needs.
type Store interface {
	Exists(ctx context.Context, phone string, bvnHash []byte) (bool, error)
	CreateCustomer(ctx context.Context, n postgres.NewCustomer) error
	SetDepositAccount(ctx context.Context, id uuid.UUID, code string) error
	CustomersWithoutAccount(ctx context.Context, limit int) ([]uuid.UUID, error)
	GetCustomer(ctx context.Context, id uuid.UUID) (domain.Customer, error)
	CustomerIDByPhone(ctx context.Context, phone string) (uuid.UUID, error)
	CheckCustomerPIN(ctx context.Context, id uuid.UUID, now time.Time, policy postgres.LockoutPolicy, verify func(string) bool) error
	CheckStaffPassword(ctx context.Context, email string, now time.Time, policy postgres.LockoutPolicy, verify func(string) bool) (postgres.StaffMember, error)
	ReleaseBureauSubject(ctx context.Context, id uuid.UUID, caller, consentRef string) (postgres.BureauSubject, error)
	Audit(ctx context.Context, actor, action string, subject *uuid.UUID, detail map[string]any) error
}
