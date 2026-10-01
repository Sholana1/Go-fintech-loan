// Package identityclient adapts the identity service's gRPC API to the
// app.Identity port.
//
// Why synchronous gRPC: assessment and acceptance need the customer's
// current KYC status at the moment of the decision; a cached or replicated
// copy could approve a customer who has just been blocked.
package identityclient

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	identityv1 "bankplatform.internal/gen/bank/identity/v1"
	"bankplatform.internal/services/lending/app"
)

// Client implements app.Identity. It does not retry: GetCustomer is cheap to
// repeat at the assessment level (which has its own backoff), and
// VerifyCustomerPin must not be retried because each attempt counts toward
// the lockout.
type Client struct {
	identity identityv1.IdentityServiceClient
}

func New(identity identityv1.IdentityServiceClient) *Client { return &Client{identity: identity} }

func (c *Client) GetCustomer(ctx context.Context, id uuid.UUID) (app.Customer, error) {
	out, err := c.identity.GetCustomer(ctx, &identityv1.GetCustomerRequest{CustomerId: id.String()})
	if err != nil {
		return app.Customer{}, err
	}
	dob, err := time.Parse(time.DateOnly, out.GetDateOfBirth())
	if err != nil {
		return app.Customer{}, fmt.Errorf("identity returned an invalid date of birth: %w", err)
	}
	return app.Customer{
		ID:                 id,
		Active:             out.GetStatus() == identityv1.CustomerStatus_CUSTOMER_STATUS_ACTIVE,
		KYCVerified:        out.GetKycStatus() == identityv1.KycStatus_KYC_STATUS_VERIFIED,
		KYCTier:            int(out.GetKycTier()),
		FullName:           out.GetFullName(),
		DateOfBirth:        dob,
		DepositAccountCode: out.GetDepositAccountCode(),
	}, nil
}

func (c *Client) VerifyPIN(ctx context.Context, id uuid.UUID, pin string) (verified, locked bool, err error) {
	out, err := c.identity.VerifyCustomerPin(ctx, &identityv1.VerifyCustomerPinRequest{CustomerId: id.String(), Pin: pin})
	if err != nil {
		return false, false, err
	}
	return out.GetVerified(), out.GetLocked(), nil
}

func (c *Client) BureauSubject(ctx context.Context, id uuid.UUID, consentRef uuid.UUID) (app.BureauSubject, error) {
	out, err := c.identity.GetCreditBureauSubject(ctx, &identityv1.GetCreditBureauSubjectRequest{CustomerId: id.String(), ConsentRef: consentRef.String()})
	if err != nil {
		return app.BureauSubject{}, err
	}
	dob, err := time.Parse(time.DateOnly, out.GetDateOfBirth())
	if err != nil {
		return app.BureauSubject{}, fmt.Errorf("identity returned an invalid date of birth: %w", err)
	}
	return app.BureauSubject{BVN: out.GetBvn(), FullName: out.GetFullName(), DateOfBirth: dob}, nil
}

var _ app.Identity = (*Client)(nil)
