// Package secrets holds identity's cryptographic handling of credentials and
// the BVN.
//
//   - PINs and staff passwords are hashed with argon2id. A 6-digit PIN has
//     little entropy, so hashing only slows an offline attack on a stolen
//     table; the real control against guessing is the lockout counter kept
//     in PostgreSQL.
//   - The BVN is stored twice in protected form: a keyed HMAC for uniqueness
//     lookups, and AES-256-GCM ciphertext for the one use that needs the
//     value (a consented credit-bureau enquiry).
//
// Keys are supplied by configuration. In production they must come from a
// managed key service; that integration is a launch dependency.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// HashParams are argon2id cost parameters.
type HashParams struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
}

// DefaultHashParams follows the OWASP minimum recommendation for argon2id
// (19 MiB, 2 iterations, 1 thread).
var DefaultHashParams = HashParams{Time: 2, Memory: 19 * 1024, Threads: 1}

const (
	saltLen = 16
	keyLen  = 32
)

// Hash returns a self-describing argon2id encoding of secret.
func Hash(secret string, p HashParams) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(secret), salt, p.Time, p.Memory, p.Threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether secret matches the encoded hash, in constant time
// with respect to the derived key.
func Verify(secret, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var p HashParams
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Sealer protects BVNs.
type Sealer struct {
	hmacKey []byte
	aead    cipher.AEAD
}

// NewSealer takes two independent 32-byte keys.
func NewSealer(hmacKey, encKey []byte) (*Sealer, error) {
	if len(hmacKey) != 32 || len(encKey) != 32 {
		return nil, errors.New("secrets: BVN keys must be 32 bytes each")
	}
	if subtle.ConstantTimeCompare(hmacKey, encKey) == 1 {
		return nil, errors.New("secrets: BVN HMAC and encryption keys must differ")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{hmacKey: hmacKey, aead: aead}, nil
}

// Fingerprint returns the keyed hash used for uniqueness.
func (s *Sealer) Fingerprint(bvn string) []byte {
	m := hmac.New(sha256.New, s.hmacKey)
	m.Write([]byte(bvn))
	return m.Sum(nil)
}

// Seal encrypts the BVN with a random nonce prepended to the ciphertext.
func (s *Sealer) Seal(bvn string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(bvn), nil), nil
}

// Open decrypts a sealed BVN.
func (s *Sealer) Open(sealed []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("secrets: sealed value too short")
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", errors.New("secrets: cannot open sealed value")
	}
	return string(plain), nil
}

// DecodeKey decodes a base64 32-byte key from configuration.
func DecodeKey(b64 string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(k) != 32 {
		return nil, errors.New("secrets: key must be base64 of exactly 32 bytes")
	}
	return k, nil
}
