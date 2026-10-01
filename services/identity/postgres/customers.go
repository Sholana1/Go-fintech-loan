package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/identity/domain"
)

// NewCustomer is everything written at registration.
type NewCustomer struct {
	Customer      domain.Customer
	BVNHash       []byte
	BVNCiphertext []byte
	PINHash       string
	Provider      string
	ProviderRef   string
	Outcome       string
}

// CreateCustomer writes the customer, credential, KYC check and audit entry
// in one transaction. The unique constraints on phone and BVN fingerprint
// are what guarantee one customer per phone and per BVN under concurrency.
func (s *Store) CreateCustomer(ctx context.Context, n NewCustomer) error {
	return s.tx.InTx(ctx, func(tx pgx.Tx) error {
		c := n.Customer
		_, err := tx.Exec(ctx, `
			INSERT INTO identity.customers (customer_id, phone, full_name, date_of_birth, bvn_hash, bvn_ciphertext, kyc_status, kyc_tier, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			c.ID, c.Phone, c.FullName, c.DateOfBirth, n.BVNHash, n.BVNCiphertext, string(c.KYCStatus), c.KYCTier, string(c.Status))
		if pgxutil.IsUniqueViolation(err, "") {
			return domain.ErrAlreadyRegistered
		}
		if err != nil {
			return fmt.Errorf("insert customer: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity.credentials (customer_id, pin_hash) VALUES ($1,$2)`, c.ID, n.PINHash); err != nil {
			return fmt.Errorf("insert credential: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.kyc_checks (check_id, customer_id, provider, provider_ref, outcome)
			VALUES ($1,$2,$3,$4,$5)`, uuid.New(), c.ID, n.Provider, n.ProviderRef, n.Outcome); err != nil {
			return fmt.Errorf("insert kyc check: %w", err)
		}
		return audit(ctx, tx, "identity", "CUSTOMER_REGISTERED", &c.ID, map[string]any{"kyc_tier": c.KYCTier, "provider": n.Provider})
	})
}

// Exists reports whether a customer already uses this phone or BVN.
func (s *Store) Exists(ctx context.Context, phone string, bvnHash []byte) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity.customers WHERE phone = $1 OR bvn_hash = $2)`, phone, bvnHash).Scan(&exists)
	return exists, err
}

func (s *Store) SetDepositAccount(ctx context.Context, id uuid.UUID, code string) error {
	_, err := s.pool.Exec(ctx, `UPDATE identity.customers SET deposit_account_code = $2 WHERE customer_id = $1`, id, code)
	return err
}

// CustomersWithoutAccount lists customers whose ledger account is still to
// be opened, oldest first.
func (s *Store) CustomersWithoutAccount(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT customer_id FROM identity.customers
		 WHERE deposit_account_code IS NULL ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func (s *Store) GetCustomer(ctx context.Context, id uuid.UUID) (domain.Customer, error) {
	var (
		c           domain.Customer
		kyc, status string
		accountCode *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT customer_id, phone, full_name, date_of_birth, kyc_status, kyc_tier, status, deposit_account_code
		  FROM identity.customers WHERE customer_id = $1`, id).
		Scan(&c.ID, &c.Phone, &c.FullName, &c.DateOfBirth, &kyc, &c.KYCTier, &status, &accountCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Customer{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Customer{}, err
	}
	c.KYCStatus, c.Status = domain.KYCStatus(kyc), domain.CustomerStatus(status)
	if accountCode != nil {
		c.DepositAccountCode = *accountCode
	}
	return c, nil
}

// CustomerIDByPhone resolves a login identifier.
func (s *Store) CustomerIDByPhone(ctx context.Context, phone string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT customer_id FROM identity.customers WHERE phone = $1`, phone).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, domain.ErrNotFound
	}
	return id, err
}

// BureauSubject is what a credit bureau enquiry needs.
type BureauSubject struct {
	BVNCiphertext []byte
	FullName      string
	DateOfBirth   time.Time
}

// ReleaseBureauSubject returns the sealed BVN and writes the audit record of
// the release in the same transaction: there is no release without a record.
func (s *Store) ReleaseBureauSubject(ctx context.Context, id uuid.UUID, caller, consentRef string) (BureauSubject, error) {
	var out BureauSubject
	err := s.tx.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT bvn_ciphertext, full_name, date_of_birth FROM identity.customers WHERE customer_id = $1`, id).
			Scan(&out.BVNCiphertext, &out.FullName, &out.DateOfBirth)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, caller, "BUREAU_SUBJECT_RELEASED", &id, map[string]any{"consent_ref": consentRef})
	})
	return out, err
}
