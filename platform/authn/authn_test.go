package authn

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func newPair(t *testing.T, now func() time.Time) (*Signer, *Verifier) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner(priv, "identity", 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(pub, "identity", now)
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	now := time.Now
	s, v := newPair(t, now)
	sub := uuid.NewString()
	tok, exp, err := s.Sign(sub, KindStaff, []string{RoleOpsMaker})
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) > 16*time.Minute {
		t.Fatalf("unexpected expiry %v", exp)
	}
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != sub || p.Kind != KindStaff || !p.HasRole(RoleOpsMaker) || p.HasRole(RoleOpsChecker) || p.TokenID == "" {
		t.Fatalf("unexpected principal %+v", p)
	}
}

func TestVerifyRejections(t *testing.T) {
	clock := time.Now()
	now := func() time.Time { return clock }
	s, v := newPair(t, now)
	sub := uuid.NewString()
	good, _, _ := s.Sign(sub, KindCustomer, nil)

	t.Run("expired", func(t *testing.T) {
		clock = clock.Add(16 * time.Minute)
		defer func() { clock = clock.Add(-16 * time.Minute) }()
		if _, err := v.Verify(good); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("want ErrInvalidToken, got %v", err)
		}
	})

	t.Run("signed by another key", func(t *testing.T) {
		other, _ := newPair(t, now)
		tok, _, _ := other.Sign(sub, KindCustomer, nil)
		if _, err := v.Verify(tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("want ErrInvalidToken, got %v", err)
		}
	})

	t.Run("alg none is refused", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
			"sub": sub, "knd": "customer", "iss": "identity", "aud": audience,
			"exp": clock.Add(time.Hour).Unix(),
		})
		signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(signed); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("want ErrInvalidToken, got %v", err)
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		parts := strings.Split(good, ".")
		parts[1] = parts[1][:len(parts[1])-2] + "AA"
		if _, err := v.Verify(strings.Join(parts, ".")); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("want ErrInvalidToken, got %v", err)
		}
	})

	t.Run("customer token with roles", func(t *testing.T) {
		tok, _, _ := s.Sign(sub, KindCustomer, []string{RoleOpsChecker})
		if _, err := v.Verify(tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("want ErrInvalidToken, got %v", err)
		}
	})

	t.Run("non-uuid subject", func(t *testing.T) {
		tok, _, _ := s.Sign("admin", KindStaff, nil)
		if _, err := v.Verify(tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("want ErrInvalidToken, got %v", err)
		}
	})
}
