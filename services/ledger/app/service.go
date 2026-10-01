// Package app is the ledger's application layer: it authorises the calling
// workload, validates the request shape, and delegates to the posting
// transactions. It owns no SQL and no transport types.
//
// Inputs: domain commands plus the caller's workload identity.
// Outputs: domain results or domain errors.
// Dependencies: a Store (the postgres package) and a Metrics sink.
// Consistency boundary: none of its own; each Store method is one atomic
// database transaction.
package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/money"
	"bankplatform.internal/services/ledger/domain"
	"bankplatform.internal/services/ledger/postgres"
)

// Store is the persistence the service needs. It is defined here, next to
// its only consumer, and implemented by postgres.Store.
type Store interface {
	PostJournal(ctx context.Context, cmd postgres.PostCommand) (domain.Journal, error)
	PlaceHold(ctx context.Context, cmd postgres.HoldCommand) (postgres.HoldResult, error)
	CaptureHold(ctx context.Context, cmd postgres.CaptureCommand) (domain.Journal, error)
	ReleaseHold(ctx context.Context, ref domain.HoldRef) (domain.HoldStatus, bool, error)
	OpenCustomerDeposit(ctx context.Context, cmd postgres.OpenAccountCommand) (bool, error)
	GetBalance(ctx context.Context, code string) (domain.Balance, error)
	GetJournal(ctx context.Context, ref domain.PostingRef) (domain.Journal, error)
}

// Metrics receives operational signals. Implementations must be cheap and
// must not block.
type Metrics interface {
	JournalPosted(journalType string, duplicate bool, d time.Duration)
	PostingRejected(journalType string, reason string)
}

// Actions that are not journal types.
const (
	ActionOpenAccount = "OPEN_ACCOUNT"
	ActionRead        = "READ"
	holdActionPrefix  = "HOLD:"
)

// Service implements the ledger use cases.
type Service struct {
	store    Store
	rights   map[string]map[string]bool
	policies map[string]postgres.JournalPolicy
	metrics  Metrics
}

// NewService builds the service. rights maps caller -> action -> allowed;
// policies gives the permitted shape of each journal type. Both are loaded
// from the database at startup and change only through migrations.
func NewService(store Store, rights map[string]map[string]bool, policies map[string]postgres.JournalPolicy, metrics Metrics) *Service {
	return &Service{store: store, rights: rights, policies: policies, metrics: metrics}
}

// checkShape enforces the journal type's policy: which system accounts it
// may touch and how many customer accounts. This is what stops a service
// with a legitimate posting right from using it to move money somewhere
// that journal type has no business reaching.
func (s *Service) checkShape(journalType string, lines []domain.Line) error {
	pol, ok := s.policies[journalType]
	if !ok {
		return fmt.Errorf("%w: journal type %s has no policy", domain.ErrNotAuthorised, journalType)
	}
	customers := map[string]struct{}{}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l.AccountCode, "SYS:"):
			if !pol.SystemAccounts[l.AccountCode] {
				return fmt.Errorf("%w: %s may not touch %s", domain.ErrNotAuthorised, journalType, l.AccountCode)
			}
		case strings.HasPrefix(l.AccountCode, "CUST:"):
			customers[l.AccountCode] = struct{}{}
		default:
			return fmt.Errorf("%w: unknown account namespace in %s", domain.ErrInvalid, l.AccountCode)
		}
	}
	if len(customers) > pol.MaxCustomerAccounts {
		return fmt.Errorf("%w: %s may touch at most %d customer account(s)", domain.ErrNotAuthorised, journalType, pol.MaxCustomerAccounts)
	}
	return nil
}

func (s *Service) authorise(caller, action string) error {
	if caller == "" || !s.rights[caller][action] {
		return fmt.Errorf("%w: %q may not %s", domain.ErrNotAuthorised, caller, action)
	}
	return nil
}

// PostJournal posts a balanced journal on behalf of caller.
func (s *Service) PostJournal(ctx context.Context, caller string, cmd postgres.PostCommand) (domain.Journal, error) {
	if err := s.validatePost(caller, cmd); err != nil {
		s.metrics.PostingRejected(cmd.JournalType, Reason(err))
		return domain.Journal{}, err
	}
	cmd.Caller = caller
	start := time.Now()
	j, err := s.store.PostJournal(ctx, cmd)
	if err != nil {
		s.metrics.PostingRejected(cmd.JournalType, Reason(err))
		return domain.Journal{}, err
	}
	s.metrics.JournalPosted(cmd.JournalType, j.AlreadyPosted, time.Since(start))
	return j, nil
}

func (s *Service) validatePost(caller string, cmd postgres.PostCommand) error {
	if !domain.ValidJournalType(cmd.JournalType) {
		return fmt.Errorf("%w: journal type is required", domain.ErrInvalid)
	}
	if err := s.authorise(caller, cmd.JournalType); err != nil {
		return err
	}
	if err := cmd.Ref.Validate(); err != nil {
		return err
	}
	if len(cmd.Narrative) > 200 {
		return fmt.Errorf("%w: narrative is limited to 200 characters", domain.ErrInvalid)
	}
	if err := domain.ValidateLines(cmd.Lines); err != nil {
		return err
	}
	return s.checkShape(cmd.JournalType, cmd.Lines)
}

// PlaceHold reserves funds on behalf of caller.
func (s *Service) PlaceHold(ctx context.Context, caller string, cmd postgres.HoldCommand) (postgres.HoldResult, error) {
	if err := cmd.Ref.Validate(); err != nil {
		return postgres.HoldResult{}, err
	}
	if err := s.authorise(caller, holdActionPrefix+cmd.Ref.OpType); err != nil {
		return postgres.HoldResult{}, err
	}
	// Holds exist to reserve customer funds; system accounts are never held.
	if !domain.ValidAccountCode(cmd.AccountCode) || !strings.HasPrefix(cmd.AccountCode, "CUST:") || !cmd.Amount.IsPositive() {
		return postgres.HoldResult{}, fmt.Errorf("%w: hold needs an account and a positive amount", domain.ErrInvalid)
	}
	cmd.Caller = caller
	return s.store.PlaceHold(ctx, cmd)
}

// CaptureHold consumes a hold with a journal on behalf of caller.
func (s *Service) CaptureHold(ctx context.Context, caller string, cmd postgres.CaptureCommand) (domain.Journal, error) {
	if err := cmd.Hold.Validate(); err != nil {
		return domain.Journal{}, err
	}
	// The caller must hold both rights: to use this kind of hold and to post
	// this type of journal.
	if err := s.authorise(caller, holdActionPrefix+cmd.Hold.OpType); err != nil {
		return domain.Journal{}, err
	}
	if err := s.validatePost(caller, cmd.Post); err != nil {
		s.metrics.PostingRejected(cmd.Post.JournalType, Reason(err))
		return domain.Journal{}, err
	}
	cmd.Post.Caller = caller
	start := time.Now()
	j, err := s.store.CaptureHold(ctx, cmd)
	if err != nil {
		s.metrics.PostingRejected(cmd.Post.JournalType, Reason(err))
		return domain.Journal{}, err
	}
	s.metrics.JournalPosted(cmd.Post.JournalType, j.AlreadyPosted, time.Since(start))
	return j, nil
}

// ReleaseHold closes a hold without posting.
func (s *Service) ReleaseHold(ctx context.Context, caller string, ref domain.HoldRef) (domain.HoldStatus, bool, error) {
	if err := ref.Validate(); err != nil {
		return "", false, err
	}
	if err := s.authorise(caller, holdActionPrefix+ref.OpType); err != nil {
		return "", false, err
	}
	return s.store.ReleaseHold(ctx, ref)
}

// OpenCustomerDeposit opens a customer's deposit account.
func (s *Service) OpenCustomerDeposit(ctx context.Context, caller, code string, currency money.Currency, ownerID uuid.UUID) (bool, error) {
	if err := s.authorise(caller, ActionOpenAccount); err != nil {
		return false, err
	}
	if !domain.ValidAccountCode(code) || !strings.HasPrefix(code, "CUST:") || ownerID == uuid.Nil {
		return false, fmt.Errorf("%w: a customer account code and owner are required", domain.ErrInvalid)
	}
	if _, err := money.New(0, currency); err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrInvalid, err)
	}
	return s.store.OpenCustomerDeposit(ctx, postgres.OpenAccountCommand{Code: code, Currency: currency, OwnerID: ownerID})
}

// GetBalance returns an account's balance to callers with the READ right.
func (s *Service) GetBalance(ctx context.Context, caller, code string) (domain.Balance, error) {
	if err := s.authorise(caller, ActionRead); err != nil {
		return domain.Balance{}, err
	}
	if !domain.ValidAccountCode(code) {
		return domain.Balance{}, fmt.Errorf("%w: account code is required", domain.ErrInvalid)
	}
	return s.store.GetBalance(ctx, code)
}

// GetJournal returns a journal by reference to callers with the READ right.
func (s *Service) GetJournal(ctx context.Context, caller string, ref domain.PostingRef) (domain.Journal, error) {
	if err := s.authorise(caller, ActionRead); err != nil {
		return domain.Journal{}, err
	}
	if err := ref.Validate(); err != nil {
		return domain.Journal{}, err
	}
	return s.store.GetJournal(ctx, ref)
}
