package lendingtest

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"bankplatform.internal/services/lending/app"
)

// FaultyLedger wraps the real ledger client and injects faults on demand.
//
//	FailBefore(op, n): the next n calls of op fail WITHOUT reaching the ledger
//	                   (the ledger is unreachable).
//	FailAfter(op, n):  the next n calls of op reach the ledger and take
//	                   effect, but the caller gets an error (the response was
//	                   lost, or the process died before it could act on it).
type FaultyLedger struct {
	inner app.Ledger

	mu     sync.Mutex
	before map[string]int
	after  map[string]int
	calls  map[string]int
}

// ErrInjected is the error returned for injected faults.
var ErrInjected = errors.New("injected ledger fault")

func (f *FaultyLedger) FailBefore(op string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.before == nil {
		f.before = map[string]int{}
	}
	f.before[op] = n
}

func (f *FaultyLedger) FailAfter(op string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.after == nil {
		f.after = map[string]int{}
	}
	f.after[op] = n
}

// Calls returns how many times op reached the real ledger.
func (f *FaultyLedger) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

func (f *FaultyLedger) gate(op string) (failBefore, failAfter bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.before[op] > 0 {
		f.before[op]--
		return true, false
	}
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[op]++
	if f.after[op] > 0 {
		f.after[op]--
		return false, true
	}
	return false, false
}

func (f *FaultyLedger) Post(ctx context.Context, req app.PostRequest) (app.PostedJournal, error) {
	before, after := f.gate("Post")
	if before {
		return app.PostedJournal{}, ErrInjected
	}
	out, err := f.inner.Post(ctx, req)
	if after && err == nil {
		return app.PostedJournal{}, ErrInjected
	}
	return out, err
}

func (f *FaultyLedger) PlaceHold(ctx context.Context, opType string, opID uuid.UUID, accountCode string, amountMinor int64) error {
	before, after := f.gate("PlaceHold")
	if before {
		return ErrInjected
	}
	err := f.inner.PlaceHold(ctx, opType, opID, accountCode, amountMinor)
	if after && err == nil {
		return ErrInjected
	}
	return err
}

func (f *FaultyLedger) CaptureHold(ctx context.Context, holdOpType string, holdOpID uuid.UUID, req app.PostRequest) (app.PostedJournal, error) {
	before, after := f.gate("CaptureHold")
	if before {
		return app.PostedJournal{}, ErrInjected
	}
	out, err := f.inner.CaptureHold(ctx, holdOpType, holdOpID, req)
	if after && err == nil {
		return app.PostedJournal{}, ErrInjected
	}
	return out, err
}

func (f *FaultyLedger) ReleaseHold(ctx context.Context, opType string, opID uuid.UUID) error {
	before, after := f.gate("ReleaseHold")
	if before {
		return ErrInjected
	}
	err := f.inner.ReleaseHold(ctx, opType, opID)
	if after && err == nil {
		return ErrInjected
	}
	return err
}

func (f *FaultyLedger) Available(ctx context.Context, accountCode string) (int64, error) {
	if before, _ := f.gate("Available"); before {
		return 0, ErrInjected
	}
	return f.inner.Available(ctx, accountCode)
}

func (f *FaultyLedger) Posted(ctx context.Context, accountCode string) (int64, error) {
	if before, _ := f.gate("Posted"); before {
		return 0, ErrInjected
	}
	return f.inner.Posted(ctx, accountCode)
}

func (f *FaultyLedger) Journal(ctx context.Context, ref app.JournalRef) (app.LedgerJournal, error) {
	if before, _ := f.gate("Journal"); before {
		return app.LedgerJournal{}, ErrInjected
	}
	return f.inner.Journal(ctx, ref)
}

// PlaceHoldDirect places a hold through the real ledger client, bypassing
// fault injection. Tests use it to construct a precise crash point.
func (f *FaultyLedger) PlaceHoldDirect(ctx context.Context, opType string, opID uuid.UUID, accountCode string, amountMinor int64) error {
	return f.inner.PlaceHold(ctx, opType, opID, accountCode, amountMinor)
}
