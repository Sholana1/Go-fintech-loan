package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"bankplatform.internal/services/lending/domain"
)

// Destination types for loan proceeds.
const (
	// DestinationDeposit leaves the proceeds in the customer's own deposit
	// account with us. This is the default and completes at T3.
	DestinationDeposit = "DEPOSIT_ACCOUNT"
	// DestinationExternal additionally sends the proceeds to the customer's
	// own account at another bank. The loan is still booked to the deposit
	// account first (T3); the external leg is a separate payout (T4) whose
	// timing depends on the provider and the receiving bank.
	DestinationExternal = "EXTERNAL_BANK_ACCOUNT"
)

// Destination says where the customer wants the proceeds.
type Destination struct {
	Type          string `json:"type"`
	BankCode      string `json:"bank_code,omitempty"`
	AccountNumber string `json:"account_number,omitempty"`
}

var (
	bankCodePattern      = regexp.MustCompile(`^\d{3,6}$`)
	accountNumberPattern = regexp.MustCompile(`^\d{10}$`)
)

func validateDestination(d Destination) error {
	switch d.Type {
	case DestinationDeposit:
		if d.BankCode != "" || d.AccountNumber != "" {
			return fmt.Errorf("%w: a deposit-account destination takes no bank details", domain.ErrValidation)
		}
	case DestinationExternal:
		if !bankCodePattern.MatchString(d.BankCode) || !accountNumberPattern.MatchString(d.AccountNumber) {
			return fmt.Errorf("%w: bank_code must be 3-6 digits and account_number 10 digits", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("%w: unknown destination type", domain.ErrValidation)
	}
	return nil
}

// resolveOwnAccount performs the name enquiry and requires the account to be
// in the customer's own verified name. Loan proceeds are never sent to a
// third party.
func (s *Service) resolveOwnAccount(ctx context.Context, customer Customer, d Destination) (ResolvedAccount, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.PayoutTimeout)
	defer cancel()
	start := time.Now()
	resolved, err := s.payouts.ResolveAccount(ctx, d.BankCode, d.AccountNumber)
	switch {
	case errors.Is(err, ErrAccountNotResolved):
		s.metrics.ProviderCall(s.payouts.Name(), "resolve", "not_found", time.Since(start))
		return ResolvedAccount{}, domain.ErrDestinationRejected
	case err != nil:
		s.metrics.ProviderCall(s.payouts.Name(), "resolve", "error", time.Since(start))
		return ResolvedAccount{}, fmt.Errorf("%w: name enquiry: %v", domain.ErrDependencyUnavailable, err)
	}
	s.metrics.ProviderCall(s.payouts.Name(), "resolve", "ok", time.Since(start))
	if !NamesMatch(customer.FullName, resolved.AccountName) {
		return ResolvedAccount{}, domain.ErrDestinationRejected
	}
	return resolved, nil
}

// NamesMatch reports whether an account name returned by a name enquiry
// belongs to the same person as the verified customer name.
//
// Rule: compare the names as sets of words, ignoring case, order and
// punctuation. Every word of the shorter name must appear in the longer one,
// and at least two words must match. "ADA TEST CUSTOMER" matches "Customer,
// Ada Test" and "Ada Customer"; it does not match "Ada Okafor".
//
// This is deliberately strict. A mismatch sends the customer to their
// deposit account instead, which is always available.
func NamesMatch(verified, enquired string) bool {
	a, b := nameWords(verified), nameWords(enquired)
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(a) < 2 {
		return false
	}
	for w := range a {
		if !b[w] {
			return false
		}
	}
	return true
}

func nameWords(s string) map[string]bool {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) })
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}
