// Package domain holds identity's vocabulary and input rules: what a valid
// phone number, PIN, BVN and date of birth look like, and the KYC states a
// customer can be in. It performs no I/O.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/bizdate"
)

var (
	ErrInvalid            = errors.New("invalid input")
	ErrAlreadyRegistered  = errors.New("customer already registered")
	ErrIdentityNotMatched = errors.New("identity could not be verified")
	ErrBadCredentials     = errors.New("invalid credentials")
	ErrLocked             = errors.New("credential temporarily locked")
	ErrNotFound           = errors.New("not found")
	ErrNotAuthorised      = errors.New("not authorised")
	ErrBlocked            = errors.New("customer is blocked")
)

type KYCStatus string

const (
	KYCPending  KYCStatus = "PENDING"
	KYCVerified KYCStatus = "VERIFIED"
	KYCRejected KYCStatus = "REJECTED"
)

type CustomerStatus string

const (
	CustomerActive  CustomerStatus = "ACTIVE"
	CustomerBlocked CustomerStatus = "BLOCKED"
)

// Customer is the KYC view of a customer. It never carries the BVN.
type Customer struct {
	ID                 uuid.UUID
	Phone              string
	FullName           string
	DateOfBirth        time.Time
	KYCStatus          KYCStatus
	KYCTier            int
	Status             CustomerStatus
	DepositAccountCode string // empty until the ledger account exists
}

// Registration is the validated input to customer registration.
type Registration struct {
	Phone       string
	FullName    string
	DateOfBirth time.Time
	BVN         string
	PIN         string
}

var (
	phonePattern = regexp.MustCompile(`^\+234[789][01]\d{8}$`)
	bvnPattern   = regexp.MustCompile(`^\d{11}$`)
	pinPattern   = regexp.MustCompile(`^\d{6}$`)
	namePattern  = regexp.MustCompile(`^[\p{L}][\p{L} .'-]{1,98}[\p{L}.]$`)
)

// ValidPhone reports whether s is a Nigerian mobile number in +234 format.
func ValidPhone(s string) bool { return phonePattern.MatchString(s) }

// MinimumAge is the age of contractual capacity. A product may require more.
const MinimumAge = 18

// Validate checks the registration input. today is the current business date.
func (r Registration) Validate(today time.Time) error {
	switch {
	case !phonePattern.MatchString(r.Phone):
		return fmt.Errorf("%w: phone must be a Nigerian mobile number in +234 format", ErrInvalid)
	case !namePattern.MatchString(strings.TrimSpace(r.FullName)):
		return fmt.Errorf("%w: full name is required", ErrInvalid)
	case !bvnPattern.MatchString(r.BVN):
		return fmt.Errorf("%w: BVN must be 11 digits", ErrInvalid)
	case !pinPattern.MatchString(r.PIN):
		return fmt.Errorf("%w: PIN must be 6 digits", ErrInvalid)
	case WeakPIN(r.PIN):
		return fmt.Errorf("%w: PIN is too easy to guess", ErrInvalid)
	case r.DateOfBirth.IsZero() || r.DateOfBirth.After(today):
		return fmt.Errorf("%w: date of birth is required", ErrInvalid)
	case AgeOn(r.DateOfBirth, today) < MinimumAge:
		return fmt.Errorf("%w: customers must be at least %d years old", ErrInvalid, MinimumAge)
	}
	return nil
}

// WeakPIN rejects PINs made of one repeated digit or a simple run.
func WeakPIN(pin string) bool {
	if strings.Count(pin, pin[:1]) == len(pin) {
		return true
	}
	return pin == "123456" || pin == "654321" || pin == "012345"
}

// AgeOn returns completed years between dob and on.
func AgeOn(dob, on time.Time) int { return bizdate.Age(dob, on) }

// NormaliseName lower-cases and collapses whitespace for name comparison.
func NormaliseName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
