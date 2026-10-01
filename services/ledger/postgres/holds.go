package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/money"
	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/ledger/domain"
)

// holdRow is a hold read under its row lock.
type holdRow struct {
	id        uuid.UUID
	accountID int64
	code      string
	currency  money.Currency
	amount    int64
	status    domain.HoldStatus
}

func lockHold(ctx context.Context, tx pgx.Tx, ref domain.HoldRef) (*holdRow, error) {
	h := &holdRow{}
	var cur, status string
	err := tx.QueryRow(ctx, `
		SELECT h.hold_id, h.account_id, a.code, h.currency, h.amount, h.status
		  FROM ledger.holds h JOIN ledger.accounts a USING (account_id)
		 WHERE h.op_type = $1 AND h.op_id = $2
		   FOR UPDATE OF h`, ref.OpType, ref.OpID).Scan(&h.id, &h.accountID, &h.code, &cur, &h.amount, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrHoldNotFound
	}
	if err != nil {
		return nil, err
	}
	h.currency = money.Currency(cur)
	h.status = domain.HoldStatus(status)
	return h, nil
}

// HoldCommand is a request to reserve funds.
type HoldCommand struct {
	Ref         domain.HoldRef
	AccountCode string
	Amount      money.Amount
	ExpiresAt   *time.Time
	Caller      string
}

// HoldResult describes a hold after PlaceHold.
type HoldResult struct {
	ID            uuid.UUID
	Status        domain.HoldStatus
	AlreadyPlaced bool
}

// PlaceHold reserves available funds. It is idempotent on the hold
// reference: concurrent or repeated requests with the same reference
// serialise on the account's balance lock and the second finds the first.
func (s *Store) PlaceHold(ctx context.Context, cmd HoldCommand) (HoldResult, error) {
	var out HoldResult
	err := s.tx.InTx(ctx, func(tx pgx.Tx) error {
		byCode, _, err := lockAccounts(ctx, tx, []string{cmd.AccountCode})
		if err != nil {
			return err
		}
		a, ok := byCode[cmd.AccountCode]
		if !ok {
			return fmt.Errorf("%w: %s", domain.ErrAccountNotFound, cmd.AccountCode)
		}

		// Look for an existing hold only after taking the balance lock, so
		// that a concurrent placement with the same reference has either
		// committed (and is visible) or has not started.
		var (
			existingID      uuid.UUID
			existingAccount int64
			existingAmount  int64
			existingStatus  string
		)
		err = tx.QueryRow(ctx, `
			SELECT hold_id, account_id, amount, status FROM ledger.holds
			 WHERE op_type = $1 AND op_id = $2`, cmd.Ref.OpType, cmd.Ref.OpID,
		).Scan(&existingID, &existingAccount, &existingAmount, &existingStatus)
		switch {
		case err == nil:
			if existingAccount != a.id || existingAmount != cmd.Amount.Minor() {
				return domain.ErrRefReused
			}
			out = HoldResult{ID: existingID, Status: domain.HoldStatus(existingStatus), AlreadyPlaced: true}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		if a.currency != cmd.Amount.Currency() {
			return domain.ErrCurrencyMismatch
		}
		if a.status != "ACTIVE" {
			return fmt.Errorf("%w: %s is %s", domain.ErrAccountNotActive, a.code, a.status)
		}
		if a.floor != nil && a.posted-a.held-cmd.Amount.Minor() < *a.floor {
			return fmt.Errorf("%w: %s", domain.ErrInsufficientFunds, a.code)
		}

		id := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger.holds (hold_id, account_id, currency, amount, op_type, op_id, expires_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, a.id, string(a.currency), cmd.Amount.Minor(), cmd.Ref.OpType, cmd.Ref.OpID, cmd.ExpiresAt, cmd.Caller); err != nil {
			if pgxutil.Constraint(err) == "available_not_below_floor" {
				return fmt.Errorf("%w (database constraint)", domain.ErrInsufficientFunds)
			}
			return fmt.Errorf("insert hold: %w", err)
		}
		out = HoldResult{ID: id, Status: domain.HoldActive}
		return nil
	})
	return out, err
}

// CaptureCommand closes a hold and posts the journal that consumes it.
type CaptureCommand struct {
	Hold domain.HoldRef
	Post PostCommand
}

// CaptureHold closes the hold and posts the journal in one transaction, so
// there is no moment at which the funds are neither held nor debited. The
// journal must debit the held account by no more than the hold amount; any
// remainder is released because closing the hold frees its full amount.
func (s *Store) CaptureHold(ctx context.Context, cmd CaptureCommand) (domain.Journal, error) {
	var out domain.Journal
	err := s.tx.InTx(ctx, func(tx pgx.Tx) error {
		existing, claimed, err := claimRef(ctx, tx, cmd.Post.Ref, captureFingerprint(cmd))
		if err != nil {
			return err
		}
		if !claimed {
			out = existing
			return nil
		}

		h, err := lockHold(ctx, tx, cmd.Hold)
		if err != nil {
			return err
		}
		switch h.status {
		case domain.HoldActive:
		case domain.HoldCaptured:
			// Captured under a different posting reference.
			return domain.ErrHoldCaptured
		default:
			return fmt.Errorf("%w: hold is %s", domain.ErrHoldNotActive, h.status)
		}

		var captured int64
		for _, l := range cmd.Post.Lines {
			if l.AccountCode == h.code && l.Direction == domain.Debit {
				captured += l.Amount.Minor()
			}
		}
		if captured <= 0 {
			return fmt.Errorf("%w: capture journal must debit the held account", domain.ErrInvalid)
		}
		if captured > h.amount {
			return fmt.Errorf("%w: capture of %d exceeds hold of %d", domain.ErrInvalid, captured, h.amount)
		}

		// Lock balances (in the standard order) before the hold update fires
		// its trigger, so lock acquisition order does not depend on which
		// account the hold happens to be on.
		if _, _, err := lockAccounts(ctx, tx, distinctCodes(cmd.Post.Lines)); err != nil {
			return err
		}
		// Close the hold before inserting entries: the hold's amount must
		// stop counting as held before the debit reduces posted, otherwise
		// the CHECK on balances would see the funds counted twice.
		if _, err := tx.Exec(ctx, `
			UPDATE ledger.holds SET status = 'CAPTURED', captured_amount = $2, closed_at = now()
			 WHERE hold_id = $1`, h.id, captured); err != nil {
			return fmt.Errorf("close hold: %w", err)
		}

		out, err = s.post(ctx, tx, cmd.Post)
		return err
	})
	return out, err
}

func captureFingerprint(cmd CaptureCommand) []byte {
	// Bind the hold reference into the fingerprint so one posting reference
	// cannot be replayed against a different hold.
	lines := append([]domain.Line(nil), cmd.Post.Lines...)
	return domain.Fingerprint(cmd.Post.JournalType+"|"+cmd.Hold.OpType+"|"+cmd.Hold.OpID.String(), lines)
}

// ReleaseHold closes an active hold without posting. Releasing an already
// released or expired hold is a no-op. Releasing a captured hold is an error:
// the caller believed the money was not spent, and it was.
func (s *Store) ReleaseHold(ctx context.Context, ref domain.HoldRef) (domain.HoldStatus, bool, error) {
	var (
		status  domain.HoldStatus
		already bool
	)
	err := s.tx.InTx(ctx, func(tx pgx.Tx) error {
		h, err := lockHold(ctx, tx, ref)
		if err != nil {
			return err
		}
		switch h.status {
		case domain.HoldActive:
			if _, err := tx.Exec(ctx, `
				UPDATE ledger.holds SET status = 'RELEASED', closed_at = now() WHERE hold_id = $1`, h.id); err != nil {
				return fmt.Errorf("release hold: %w", err)
			}
			status, already = domain.HoldReleased, false
		case domain.HoldCaptured:
			return domain.ErrHoldCaptured
		default:
			status, already = h.status, true
		}
		return nil
	})
	return status, already, err
}
