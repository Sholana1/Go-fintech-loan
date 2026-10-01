package secrets

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

var light = HashParams{Time: 1, Memory: 64, Threads: 1}

func TestHashAndVerify(t *testing.T) {
	h, err := Hash("482915", light)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") || strings.Contains(h, "482915") {
		t.Fatalf("unexpected encoding %q", h)
	}
	if !Verify("482915", h) || Verify("482916", h) || Verify("482915", "garbage") || Verify("", h) {
		t.Fatal("verification is wrong")
	}
	h2, _ := Hash("482915", light)
	if h == h2 {
		t.Fatal("two hashes of the same secret must differ (random salt)")
	}
}

func keys(t *testing.T) ([]byte, []byte) {
	a, b := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(a); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestSealer(t *testing.T) {
	hk, ek := keys(t)
	s, err := NewSealer(hk, ek)
	if err != nil {
		t.Fatal(err)
	}
	const bvn = "22345678901"
	sealed, err := s.Seal(bvn)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(bvn)) {
		t.Fatal("ciphertext contains the plaintext")
	}
	again, _ := s.Seal(bvn)
	if bytes.Equal(sealed, again) {
		t.Fatal("sealing twice must give different ciphertexts (random nonce)")
	}
	opened, err := s.Open(sealed)
	if err != nil || opened != bvn {
		t.Fatalf("open: %q %v", opened, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := s.Open(sealed); err == nil {
		t.Fatal("tampered ciphertext must not open")
	}

	// The fingerprint is deterministic per key and differs across keys.
	if !bytes.Equal(s.Fingerprint(bvn), s.Fingerprint(bvn)) {
		t.Fatal("fingerprint must be deterministic")
	}
	hk2, ek2 := keys(t)
	other, _ := NewSealer(hk2, ek2)
	if bytes.Equal(s.Fingerprint(bvn), other.Fingerprint(bvn)) {
		t.Fatal("fingerprint must depend on the key")
	}

	if _, err := NewSealer(hk, hk); err == nil {
		t.Fatal("identical keys must be refused")
	}
	if _, err := NewSealer(hk[:16], ek); err == nil {
		t.Fatal("short keys must be refused")
	}
}
