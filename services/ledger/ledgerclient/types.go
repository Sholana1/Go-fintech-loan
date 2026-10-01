// Package ledgerclient is the Go client other services use to call the
// ledger. Like package contract it is owned by the ledger and depends on
// nothing inside it: generated protobuf code, the stable error reasons, and
// the gRPC helpers only.
//
// What it adds over the generated client:
//
//   - Errors are classified. A caller needs to know one thing about a
//     failed call: was it REFUSED (a business answer; repeating it will not
//     help) or is the outcome UNKNOWN (timeout, unavailable; repeat with the
//     same reference)? Refusals are returned as the sentinel errors below.
//     Anything else is an unknown outcome and is returned unchanged.
//   - Transient transport failures are retried here, a bounded number of
//     times, with the same reference. That is safe because every mutating
//     ledger call is idempotent on its reference. Each retry is logged.
//     Refusals are never retried.
//
// Why synchronous gRPC: a caller needs the ledger's answer to decide its
// next step, and the ledger is the single authority on whether money may
// move. The call chain is one hop deep; the ledger calls nothing.
package ledgerclient

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Refusals. Each means the ledger understood the request and declined it.
var (
	// ErrInsufficientFunds: the account's available balance is too low.
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
	// ErrRejected: refused for a reason repeating cannot fix (inactive
	// account, not authorised, malformed, reference reused with different
	// content).
	ErrRejected = errors.New("ledger: request rejected")
	// ErrHoldNotFound: no hold exists under the reference.
	ErrHoldNotFound = errors.New("ledger: hold not found")
	// ErrHoldCaptured: the hold was already captured.
	ErrHoldCaptured = errors.New("ledger: hold already captured")
	// ErrHoldNotActive: the hold was released or expired.
	ErrHoldNotActive = errors.New("ledger: hold not active")
	// ErrNotFound: the journal or account does not exist.
	ErrNotFound = errors.New("ledger: not found")
)

// Refused reports whether err is a definite refusal by the ledger, as
// opposed to an unknown outcome.
func Refused(err error) bool {
	for _, target := range []error{ErrInsufficientFunds, ErrRejected, ErrHoldNotFound, ErrHoldCaptured, ErrHoldNotActive, ErrNotFound} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// Directions of a journal line.
const (
	Debit  = "DEBIT"
	Credit = "CREDIT"
)

// Ref identifies one posting. Posting the same Ref again returns the
// original journal; posting it with different content is refused.
type Ref struct {
	OpType string
	OpID   uuid.UUID
	OpStep string
}

// Line is one line of a journal.
type Line struct {
	AccountCode string
	Direction   string // Debit or Credit
	AmountMinor int64
}

// PostRequest asks the ledger to post one journal.
type PostRequest struct {
	Ref          Ref
	JournalType  string
	Lines        []Line
	BusinessDate *time.Time
	Narrative    string
}

// Posted identifies a posted journal.
type Posted struct {
	ID       int64
	PostedAt time.Time
	// AlreadyPosted is true when the reference had been posted before: this
	// call was a repeat and created nothing.
	AlreadyPosted bool
}

// Journal is a journal read back from the ledger.
type Journal struct {
	ID    int64
	Type  string
	Lines []Line
}

// Balance is an account's balance. Available is Posted minus Held.
type Balance struct {
	Posted    int64
	Held      int64
	Available int64
}
