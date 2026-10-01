package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/money"
	"bankplatform.internal/services/ledger/domain"
)

// OpenAccountCommand creates a customer deposit account.
type OpenAccountCommand struct {
	Code     string
	Currency money.Currency
	OwnerID  uuid.UUID
}

// OpenCustomerDeposit creates a customer deposit account (liability,
// credit-normal, floor 0) with a zero balance. Idempotent on code.
func (s *Store) OpenCustomerDeposit(ctx context.Context, cmd OpenAccountCommand) (alreadyExisted bool, err error) {
	err = s.tx.InTx(ctx, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `
			INSERT INTO ledger.accounts (code, class, normal_side, currency, gl_code, owner_type, owner_id)
			VALUES ($1, 'LIABILITY', 'CREDIT', $2, '2010', 'CUSTOMER', $3)
			ON CONFLICT (code) DO NOTHING
			RETURNING account_id`, cmd.Code, string(cmd.Currency), cmd.OwnerID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			var (
				cur   string
				owner *uuid.UUID
				class string
			)
			if err := tx.QueryRow(ctx, `SELECT currency, owner_id, class::text FROM ledger.accounts WHERE code = $1`, cmd.Code).
				Scan(&cur, &owner, &class); err != nil {
				return err
			}
			if cur != string(cmd.Currency) || owner == nil || *owner != cmd.OwnerID || class != "LIABILITY" {
				return domain.ErrAccountConflict
			}
			alreadyExisted = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert account: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger.balances (account_id, floor) VALUES ($1, 0)`, id); err != nil {
			return fmt.Errorf("insert balance: %w", err)
		}
		alreadyExisted = false
		return nil
	})
	return alreadyExisted, err
}

// GetBalance reads the authoritative balance of an account. The result is a
// point-in-time view for display and reconciliation; it is never an
// authorisation to spend.
func (s *Store) GetBalance(ctx context.Context, code string) (domain.Balance, error) {
	var (
		b     domain.Balance
		cur   string
		floor *int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT a.currency, b.posted, b.held, b.floor, b.version
		  FROM ledger.accounts a JOIN ledger.balances b USING (account_id)
		 WHERE a.code = $1`, code).Scan(&cur, &b.Posted, &b.Held, &floor, &b.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Balance{}, domain.ErrAccountNotFound
	}
	if err != nil {
		return domain.Balance{}, err
	}
	b.Currency = money.Currency(cur)
	b.Available = b.Posted
	if floor != nil {
		b.Available = b.Posted - b.Held
	}
	return b, nil
}
