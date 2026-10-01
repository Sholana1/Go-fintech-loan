package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"bankplatform.internal/services/identity/domain"
	"bankplatform.internal/services/identity/postgres"
	"bankplatform.internal/services/identity/secrets"
)

// Register creates a customer after verifying their BVN.
//
// Order matters: verify with the provider first, then write. If the provider
// call fails or is ambiguous nothing is created. If the ledger account cannot
// be opened afterwards, the customer exists without an account and
// CompletePendingAccounts finishes the job; the customer cannot borrow until
// it does.
func (s *Service) Register(ctx context.Context, reg domain.Registration) (domain.Customer, error) {
	reg.FullName = strings.TrimSpace(reg.FullName)
	now := s.now()
	if err := reg.Validate(now); err != nil {
		return domain.Customer{}, err
	}
	fingerprint := s.sealer.Fingerprint(reg.BVN)
	exists, err := s.store.Exists(ctx, reg.Phone, fingerprint)
	if err != nil {
		return domain.Customer{}, err
	}
	if exists {
		return domain.Customer{}, domain.ErrAlreadyRegistered
	}

	id := uuid.New()
	v, err := s.verifier.VerifyBVN(ctx, id.String(), reg.BVN, reg.FullName, reg.DateOfBirth)
	if err != nil {
		s.log.WarnContext(ctx, "identity provider call failed", "error", err.Error())
		return domain.Customer{}, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	if v.Outcome != OutcomeMatch {
		// No customer row is written for a failed verification; the attempt
		// is audited without personal data.
		_ = s.store.Audit(ctx, "identity", "REGISTRATION_IDENTITY_NOT_MATCHED", nil, map[string]any{"provider": v.Provider, "outcome": string(v.Outcome)})
		return domain.Customer{}, domain.ErrIdentityNotMatched
	}

	pinHash, err := secrets.Hash(reg.PIN, s.cfg.HashParams)
	if err != nil {
		return domain.Customer{}, err
	}
	sealed, err := s.sealer.Seal(reg.BVN)
	if err != nil {
		return domain.Customer{}, err
	}
	c := domain.Customer{
		ID: id, Phone: reg.Phone, FullName: reg.FullName, DateOfBirth: reg.DateOfBirth,
		KYCStatus: domain.KYCVerified, KYCTier: s.cfg.TierOnBVNMatch, Status: domain.CustomerActive,
	}
	if err := s.store.CreateCustomer(ctx, postgres.NewCustomer{
		Customer: c, BVNHash: fingerprint, BVNCiphertext: sealed, PINHash: pinHash,
		Provider: v.Provider, ProviderRef: v.ProviderRef, Outcome: string(v.Outcome),
	}); err != nil {
		return domain.Customer{}, err
	}

	if code, err := s.openAccount(ctx, id); err != nil {
		s.log.WarnContext(ctx, "ledger account not opened at registration; will be completed in the background", "customer_id", id.String(), "error", err.Error())
	} else {
		c.DepositAccountCode = code
	}
	return c, nil
}

func (s *Service) openAccount(ctx context.Context, id uuid.UUID) (string, error) {
	code, err := s.accounts.OpenCustomerDeposit(ctx, id)
	if err != nil {
		return "", err
	}
	if err := s.store.SetDepositAccount(ctx, id, code); err != nil {
		return "", err
	}
	return code, nil
}

// CompletePendingAccounts opens ledger accounts for customers who do not have
// one yet. It is run periodically; each step is idempotent.
func (s *Service) CompletePendingAccounts(ctx context.Context) error {
	ids, err := s.store.CustomersWithoutAccount(ctx, 100)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if _, err := s.openAccount(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("customer %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
