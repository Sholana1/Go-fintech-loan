// Package domain defines the ledger's vocabulary and the validation that
// needs no database: what a journal is, when its lines are well-formed, and
// the errors the service reports.
//
// Rules that depend on current balances (sufficient funds, account status)
// cannot be decided here; they are evaluated under row locks in the postgres
// package, inside the posting transaction.
package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/platform/money"
)

// Direction is the side of an entry.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// Errors returned by the ledger. Each maps to one gRPC status and one stable
// ErrorInfo reason in the grpcapi package.
var (
	ErrInvalid           = errors.New("invalid request")
	ErrUnbalanced        = errors.New("journal lines do not balance")
	ErrNotAuthorised     = errors.New("caller is not authorised for this action")
	ErrAccountNotFound   = errors.New("account not found")
	ErrAccountNotActive  = errors.New("account status does not permit this entry")
	ErrCurrencyMismatch  = errors.New("line currency differs from account currency")
	ErrInsufficientFunds = errors.New("insufficient available funds")
	ErrRefReused         = errors.New("reference was already used with different content")
	ErrHoldNotFound      = errors.New("hold not found")
	ErrHoldNotActive     = errors.New("hold is no longer active")
	ErrHoldCaptured      = errors.New("hold was already captured")
	ErrJournalNotFound   = errors.New("journal not found")
	ErrAccountConflict   = errors.New("account exists with different attributes")
)

// PostingRef links a journal to the business operation step that caused it.
// It is the idempotency key of a posting.
type PostingRef struct {
	OpType string
	OpID   uuid.UUID
	OpStep string
}

// HoldRef identifies a hold by the operation that placed it.
type HoldRef struct {
	OpType string
	OpID   uuid.UUID
}

// Line is one debit or credit against one account.
type Line struct {
	AccountCode string
	Direction   Direction
	Amount      money.Amount // strictly positive
}

var (
	tokenPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
	codePattern  = regexp.MustCompile(`^[A-Z]{3,4}:[A-Za-z0-9:_-]{1,96}$`)
)

// Validate checks the shape of a posting reference.
func (r PostingRef) Validate() error {
	if !tokenPattern.MatchString(r.OpType) || !tokenPattern.MatchString(r.OpStep) || r.OpID == uuid.Nil {
		return fmt.Errorf("%w: posting reference needs op_type, op_id and op_step", ErrInvalid)
	}
	return nil
}

// Validate checks the shape of a hold reference.
func (r HoldRef) Validate() error {
	if !tokenPattern.MatchString(r.OpType) || r.OpID == uuid.Nil {
		return fmt.Errorf("%w: hold reference needs op_type and op_id", ErrInvalid)
	}
	return nil
}

// ValidJournalType reports whether s is a well-formed journal type token.
func ValidJournalType(s string) bool { return tokenPattern.MatchString(s) }

// ValidAccountCode reports whether s is a well-formed account code.
func ValidAccountCode(s string) bool { return codePattern.MatchString(s) }

// ValidateLines enforces the request-level journal rules:
//   - at least two lines and at most MaxLines,
//   - every amount strictly positive,
//   - debits equal credits for every currency.
//
// The same balance rule is enforced again by the database at COMMIT (the
// deferred constraint trigger); checking here gives the caller a precise
// error before any lock is taken.
func ValidateLines(lines []Line) error {
	if len(lines) < 2 {
		return fmt.Errorf("%w: a journal needs at least two lines", ErrInvalid)
	}
	if len(lines) > MaxLines {
		return fmt.Errorf("%w: a journal may have at most %d lines", ErrInvalid, MaxLines)
	}
	net := map[money.Currency]int64{}
	for i, l := range lines {
		if !ValidAccountCode(l.AccountCode) {
			return fmt.Errorf("%w: line %d has an invalid account code", ErrInvalid, i)
		}
		if l.Direction != Debit && l.Direction != Credit {
			return fmt.Errorf("%w: line %d has no direction", ErrInvalid, i)
		}
		if !l.Amount.IsPositive() {
			return fmt.Errorf("%w: line %d amount must be positive", ErrInvalid, i)
		}
		delta := l.Amount.Minor()
		if l.Direction == Credit {
			delta = -delta
		}
		sum, ok := money.AddInt64(net[l.Amount.Currency()], delta)
		if !ok {
			return fmt.Errorf("%w: journal total overflows", ErrInvalid)
		}
		net[l.Amount.Currency()] = sum
	}
	for cur, n := range net {
		if n != 0 {
			return fmt.Errorf("%w: %s debits and credits differ by %d minor units", ErrUnbalanced, cur, n)
		}
	}
	return nil
}

// MaxLines bounds a journal so one request cannot hold many row locks.
const MaxLines = 32

// Fingerprint is a stable hash of the content of a posting request. It is
// stored with the posting reference so that a repeat of the reference with
// different content is detected instead of silently returning the first
// journal. Line order does not affect the fingerprint.
func Fingerprint(journalType string, lines []Line) []byte {
	sorted := make([]Line, len(lines))
	copy(sorted, lines)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.AccountCode != b.AccountCode {
			return a.AccountCode < b.AccountCode
		}
		if a.Direction != b.Direction {
			return a.Direction < b.Direction
		}
		return a.Amount.Minor() < b.Amount.Minor()
	})
	h := sha256.New()
	writeField(h, journalType)
	for _, l := range sorted {
		writeField(h, l.AccountCode)
		writeField(h, string(l.Direction))
		writeField(h, string(l.Amount.Currency()))
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(l.Amount.Minor()))
		h.Write(buf[:])
	}
	return h.Sum(nil)
}

// writeField length-prefixes s so that adjacent fields cannot be confused.
func writeField(h interface{ Write([]byte) (int, error) }, s string) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}

// BusinessDate returns the Africa/Lagos calendar date of t.
func BusinessDate(t time.Time) time.Time { return bizdate.Of(t) }

// Journal is a posted journal as returned to callers.
type Journal struct {
	ID            int64
	Type          string
	PostedAt      time.Time
	BusinessDate  time.Time
	Lines         []Line
	CreatedBy     string
	AlreadyPosted bool
}

// HoldStatus is the lifecycle state of a hold.
type HoldStatus string

const (
	HoldActive   HoldStatus = "ACTIVE"
	HoldCaptured HoldStatus = "CAPTURED"
	HoldReleased HoldStatus = "RELEASED"
	HoldExpired  HoldStatus = "EXPIRED"
)

// Balance is the authoritative balance of one account.
type Balance struct {
	Currency  money.Currency
	Posted    int64
	Held      int64
	Available int64
	Version   int64
}
