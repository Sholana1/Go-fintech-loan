package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"

	"bankplatform.internal/services/lending/domain"
)

var idemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_\-:.]{8,128}$`)

// requestHash fingerprints a request body for idempotency. A retry must
// carry the same body; the same key with a different body is an error.
func requestHash(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func validateIdemKey(key string) error {
	if !idemKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: an Idempotency-Key of 8 to 128 letters, digits or -_:. is required", domain.ErrValidation)
	}
	return nil
}

// Event payloads. They carry identifiers and amounts only: no names, phone
// numbers, account numbers or bureau data. Consumers must ignore unknown
// fields; fields are only ever added.
