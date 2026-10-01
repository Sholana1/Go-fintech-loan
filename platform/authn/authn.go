// Package authn issues and verifies the access tokens that end users
// (customers and staff) present to REST APIs.
//
// Responsibility: Ed25519-signed, short-lived JWTs. The identity service holds
// the private key and signs; every other service holds only the public key
// and verifies. No shared secret exists that would let a verifying service
// mint tokens.
//
// Scope: authentication only. Whether a principal may act on a particular
// loan or account is resource authorisation and is enforced in each service's
// use cases.
//
// Failure modes: an expired, malformed, wrongly signed or wrong-audience
// token is rejected with ErrInvalidToken. There is no fallback.
package authn

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const audience = "bankplatform-api"

// Kind distinguishes customers from staff. A token is exactly one kind.
type Kind string

const (
	KindCustomer Kind = "customer"
	KindStaff    Kind = "staff"
)

// Staff roles. Maker and checker duties are separate roles so that
// maker-checker cannot be satisfied by one role assignment.
const (
	RoleCreditReviewer = "credit_reviewer"
	RoleOpsMaker       = "ops_maker"
	RoleOpsChecker     = "ops_checker"
	RoleOpsViewer      = "ops_viewer"
)

var ErrInvalidToken = errors.New("authn: invalid token")

// Principal is the authenticated end user of a request.
type Principal struct {
	Subject string // customer_id or staff_id (UUID)
	Kind    Kind
	Roles   []string
	TokenID string // jti; recorded as acceptance evidence
}

func (p Principal) HasRole(role string) bool { return slices.Contains(p.Roles, role) }

type claims struct {
	Kind  Kind     `json:"knd"`
	Roles []string `json:"roles,omitempty"`
	jwt.RegisteredClaims
}

// Signer mints tokens. Only the identity service constructs one.
type Signer struct {
	key    ed25519.PrivateKey
	issuer string
	ttl    time.Duration
	now    func() time.Time
}

func NewSigner(key ed25519.PrivateKey, issuer string, ttl time.Duration, now func() time.Time) (*Signer, error) {
	if len(key) != ed25519.PrivateKeySize || issuer == "" || ttl <= 0 || now == nil {
		return nil, errors.New("authn: signer requires a key, issuer, positive ttl and clock")
	}
	return &Signer{key: key, issuer: issuer, ttl: ttl, now: now}, nil
}

// Sign returns a token for the subject and when it expires.
func (s *Signer) Sign(subject string, kind Kind, roles []string) (string, time.Time, error) {
	now := s.now()
	exp := now.Add(s.ttl)
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims{
		Kind:  kind,
		Roles: roles,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   subject,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
	})
	signed, err := tok.SignedString(s.key)
	return signed, exp, err
}

// Verifier checks tokens.
type Verifier struct {
	key    ed25519.PublicKey
	issuer string
	now    func() time.Time
}

func NewVerifier(key ed25519.PublicKey, issuer string, now func() time.Time) (*Verifier, error) {
	if len(key) != ed25519.PublicKeySize || issuer == "" || now == nil {
		return nil, errors.New("authn: verifier requires a key, issuer and clock")
	}
	return &Verifier{key: key, issuer: issuer, now: now}, nil
}

// Verify parses and validates a token and returns its principal.
func (v *Verifier) Verify(token string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c,
		func(*jwt.Token) (any, error) { return v.key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}), // never accept "none" or HMAC
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if _, err := uuid.Parse(c.Subject); err != nil {
		return Principal{}, fmt.Errorf("%w: subject is not a UUID", ErrInvalidToken)
	}
	if c.Kind != KindCustomer && c.Kind != KindStaff {
		return Principal{}, fmt.Errorf("%w: unknown principal kind", ErrInvalidToken)
	}
	if c.Kind == KindCustomer && len(c.Roles) > 0 {
		return Principal{}, fmt.Errorf("%w: customer tokens carry no roles", ErrInvalidToken)
	}
	return Principal{Subject: c.Subject, Kind: c.Kind, Roles: c.Roles, TokenID: c.ID}, nil
}

type principalKey struct{}

// WithPrincipal stores the authenticated principal in the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// LoadPrivateKey reads a PKCS#8 PEM Ed25519 private key.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("authn: no PEM block in private key file")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("authn: private key is not Ed25519")
	}
	return key, nil
}

// LoadPublicKey reads a PKIX PEM Ed25519 public key.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("authn: no PEM block in public key file")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("authn: public key is not Ed25519")
	}
	return key, nil
}

// EncodeKeyPair returns PEM encodings of an Ed25519 key pair; used by the
// development key generator.
func EncodeKeyPair(priv ed25519.PrivateKey) (privPEM, pubPEM []byte, err error) {
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), nil
}
