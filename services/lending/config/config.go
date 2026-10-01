// Package config loads the versioned product and credit-policy documents.
//
// The JSON files embedded here are ILLUSTRATIVE defaults for local
// development and tests. Every rate, fee, limit and threshold in them is a
// placeholder to be set by product, credit risk and compliance; none is a
// regulatory figure or a commitment. Deployments point LENDING_PRODUCT_FILE
// and LENDING_POLICY_FILE at reviewed documents instead.
//
// Both documents are validated at startup; an invalid value stops the
// service rather than mispricing a loan.
package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"bankplatform.internal/services/lending/domain"
)

//go:embed personal-loan.v1.json
var defaultProduct []byte

//go:embed credit-policy.v1.json
var defaultPolicy []byte

// Load reads the product and policy from the given files, or from the
// embedded illustrative defaults when a path is empty.
func Load(productFile, policyFile string) (domain.Product, domain.Policy, error) {
	var (
		prod domain.Product
		pol  domain.Policy
	)
	if err := decode(productFile, defaultProduct, &prod); err != nil {
		return prod, pol, fmt.Errorf("product configuration: %w", err)
	}
	if err := prod.Validate(); err != nil {
		return prod, pol, err
	}
	if err := decode(policyFile, defaultPolicy, &pol); err != nil {
		return prod, pol, fmt.Errorf("credit policy: %w", err)
	}
	if err := pol.Validate(prod); err != nil {
		return prod, pol, err
	}
	return prod, pol, nil
}

func decode(path string, fallback []byte, dst any) error {
	raw := fallback
	if path != "" {
		var err error
		if raw, err = os.ReadFile(path); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // a misspelt key must not silently fall back to zero
	return dec.Decode(dst)
}
