package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/authn"
	"bankplatform.internal/services/identity/domain"
	"bankplatform.internal/services/identity/postgres"
	"bankplatform.internal/services/identity/secrets"
)

// Session is an issued access token.
type Session struct {
	Token     string
	ExpiresAt time.Time
	Subject   uuid.UUID
}

// AuthenticateCustomer exchanges phone and PIN for an access token. Unknown
// phone numbers and wrong PINs are indistinguishable to the caller.
func (s *Service) AuthenticateCustomer(ctx context.Context, phone, pin string) (Session, error) {
	id, err := s.store.CustomerIDByPhone(ctx, phone)
	if errors.Is(err, domain.ErrNotFound) {
		secrets.Verify(pin, s.dummyHash) // equalise timing
		return Session{}, domain.ErrBadCredentials
	}
	if err != nil {
		return Session{}, err
	}
	if err := s.checkPIN(ctx, id, pin); err != nil {
		return Session{}, err
	}
	token, exp, err := s.tokens.Sign(id.String(), authn.KindCustomer, nil)
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, ExpiresAt: exp, Subject: id}, nil
}

func (s *Service) checkPIN(ctx context.Context, id uuid.UUID, pin string) error {
	err := s.store.CheckCustomerPIN(ctx, id, s.now(), s.cfg.Lockout, func(hash string) bool { return secrets.Verify(pin, hash) })
	if errors.Is(err, domain.ErrNotFound) {
		return domain.ErrBadCredentials
	}
	return err
}

// AuthenticateStaff exchanges email and password for a staff token.
//
// This is a bootstrap mechanism for local development and tests. Production
// staff authentication is single sign-on with MFA (plan section 5.4), which
// is a launch dependency.
func (s *Service) AuthenticateStaff(ctx context.Context, email, password string) (Session, error) {
	m, err := s.store.CheckStaffPassword(ctx, strings.ToLower(strings.TrimSpace(email)), s.now(), s.cfg.Lockout,
		func(hash string) bool { return secrets.Verify(password, hash) })
	if errors.Is(err, domain.ErrNotFound) {
		secrets.Verify(password, s.dummyHash)
		return Session{}, domain.ErrBadCredentials
	}
	if err != nil {
		return Session{}, err
	}
	token, exp, err := s.tokens.Sign(m.ID.String(), authn.KindStaff, m.Roles)
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, ExpiresAt: exp, Subject: m.ID}, nil
}

// CreateStaff bootstraps a staff account (development and test only; the
// command that calls it refuses to run elsewhere).
func CreateStaff(ctx context.Context, store *postgres.Store, params secrets.HashParams, email, password string, roles []string) (uuid.UUID, error) {
	if len(password) < 12 {
		return uuid.Nil, fmt.Errorf("%w: staff password must be at least 12 characters", domain.ErrInvalid)
	}
	hash, err := secrets.Hash(password, params)
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	return id, store.CreateStaff(ctx, id, strings.ToLower(strings.TrimSpace(email)), hash, roles)
}
