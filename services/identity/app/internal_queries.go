package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/services/identity/domain"
)

func allowed(list []string, caller string) bool {
	for _, c := range list {
		if c == caller && caller != "" {
			return true
		}
	}
	return false
}

// GetCustomer returns the KYC view of a customer to an internal caller.
func (s *Service) GetCustomer(ctx context.Context, caller string, id uuid.UUID) (domain.Customer, error) {
	if !allowed(s.cfg.InternalCallers, caller) {
		return domain.Customer{}, domain.ErrNotAuthorised
	}
	return s.store.GetCustomer(ctx, id)
}

// VerifyPIN is the step-up check for an internal caller. It returns
// (verified, locked).
func (s *Service) VerifyPIN(ctx context.Context, caller string, id uuid.UUID, pin string) (bool, bool, error) {
	if !allowed(s.cfg.InternalCallers, caller) {
		return false, false, domain.ErrNotAuthorised
	}
	err := s.checkPIN(ctx, id, pin)
	switch {
	case err == nil:
		return true, false, nil
	case errors.Is(err, domain.ErrLocked):
		return false, true, nil
	case errors.Is(err, domain.ErrBadCredentials), errors.Is(err, domain.ErrBlocked):
		return false, false, nil
	default:
		return false, false, err
	}
}

// BureauSubject is the decrypted subject of a credit-bureau enquiry.
type BureauSubject struct {
	BVN         string
	FullName    string
	DateOfBirth time.Time
}

// GetBureauSubject releases the BVN to an authorised caller that presents a
// consent reference. The release is audited atomically with the read.
func (s *Service) GetBureauSubject(ctx context.Context, caller string, id uuid.UUID, consentRef string) (BureauSubject, error) {
	if !allowed(s.cfg.BureauSubjectCallers, caller) {
		return BureauSubject{}, domain.ErrNotAuthorised
	}
	if strings.TrimSpace(consentRef) == "" {
		return BureauSubject{}, fmt.Errorf("%w: a consent reference is required", domain.ErrInvalid)
	}
	sub, err := s.store.ReleaseBureauSubject(ctx, id, caller, consentRef)
	if err != nil {
		return BureauSubject{}, err
	}
	bvn, err := s.sealer.Open(sub.BVNCiphertext)
	if err != nil {
		return BureauSubject{}, err
	}
	return BureauSubject{BVN: bvn, FullName: sub.FullName, DateOfBirth: sub.DateOfBirth}, nil
}

// Recipient is what a sender is shown before confirming a transfer.
type Recipient struct {
	CustomerID         uuid.UUID
	DisplayName        string
	DepositAccountCode string
}

// ResolveRecipient finds a customer who can receive money, by phone number.
//
// Anyone who cannot receive (blocked, identity not verified, account not yet
// opened) is reported as not found, the same as an unregistered number, so
// the answer reveals nothing about a customer's standing.
func (s *Service) ResolveRecipient(ctx context.Context, caller, phone string) (Recipient, error) {
	if !allowed(s.cfg.RecipientLookupCallers, caller) {
		return Recipient{}, domain.ErrNotAuthorised
	}
	if !domain.ValidPhone(phone) {
		return Recipient{}, fmt.Errorf("%w: phone must be a Nigerian mobile number in +234 format", domain.ErrInvalid)
	}
	id, err := s.store.CustomerIDByPhone(ctx, phone)
	if err != nil {
		return Recipient{}, err
	}
	c, err := s.store.GetCustomer(ctx, id)
	if err != nil {
		return Recipient{}, err
	}
	if c.Status != domain.CustomerActive || c.KYCStatus != domain.KYCVerified || c.DepositAccountCode == "" {
		return Recipient{}, domain.ErrNotFound
	}
	return Recipient{CustomerID: c.ID, DisplayName: c.FullName, DepositAccountCode: c.DepositAccountCode}, nil
}
