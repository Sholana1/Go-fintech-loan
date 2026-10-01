package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/identity/domain"
)

// LockoutPolicy bounds credential guessing.
type LockoutPolicy struct {
	MaxFailures int
	LockFor     time.Duration
}

// CheckCustomerPIN verifies a PIN under the credential's row lock.
//
// The lock makes "read failure count, verify, write failure count" atomic per
// customer, so N parallel guesses are evaluated one after another and the
// lockout threshold cannot be exceeded by racing. verify runs inside the
// transaction; it is pure CPU (argon2) and takes no other locks.
func (s *Store) CheckCustomerPIN(ctx context.Context, id uuid.UUID, now time.Time, policy LockoutPolicy, verify func(hash string) bool) error {
	// outcome carries the credential verdict out of the transaction. A wrong
	// PIN must still COMMIT (the failure counter has to persist), so the
	// transaction function returns nil and the verdict is returned after.
	var outcome error
	err := s.tx.InTx(ctx, func(tx pgx.Tx) error {
		var (
			hash        string
			failed      int
			lockedUntil *time.Time
			status      string
		)
		err := tx.QueryRow(ctx, `
			SELECT cr.pin_hash, cr.failed_attempts, cr.locked_until, c.status
			  FROM identity.credentials cr JOIN identity.customers c USING (customer_id)
			 WHERE cr.customer_id = $1
			   FOR UPDATE OF cr`, id).Scan(&hash, &failed, &lockedUntil, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != string(domain.CustomerActive) {
			return domain.ErrBlocked
		}
		if lockedUntil != nil && lockedUntil.After(now) {
			return domain.ErrLocked
		}
		if verify(hash) {
			_, err := tx.Exec(ctx, `UPDATE identity.credentials SET failed_attempts = 0, locked_until = NULL, updated_at = $2 WHERE customer_id = $1`, id, now)
			return err
		}
		failed++
		var lock *time.Time
		if failed >= policy.MaxFailures {
			t := now.Add(policy.LockFor)
			lock = &t
			failed = 0 // the lock is the penalty; counting restarts when it ends
			if err := audit(ctx, tx, "identity", "CREDENTIAL_LOCKED", &id, nil); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE identity.credentials SET failed_attempts = $2, locked_until = $3, updated_at = $4 WHERE customer_id = $1`,
			id, failed, lock, now); err != nil {
			return err
		}
		outcome = domain.ErrBadCredentials
		if lock != nil {
			outcome = domain.ErrLocked
		}
		return nil
	})
	if err != nil {
		return err
	}
	return outcome
}

// StaffMember is a staff account.
type StaffMember struct {
	ID    uuid.UUID
	Roles []string
}

// CheckStaffPassword verifies a staff password under the row lock, with the
// same lockout rule as customers.
func (s *Store) CheckStaffPassword(ctx context.Context, email string, now time.Time, policy LockoutPolicy, verify func(hash string) bool) (StaffMember, error) {
	var (
		m       StaffMember
		outcome error // see CheckCustomerPIN: a wrong password must still commit the counter
	)
	err := s.tx.InTx(ctx, func(tx pgx.Tx) error {
		var (
			hash        string
			failed      int
			lockedUntil *time.Time
			status      string
		)
		err := tx.QueryRow(ctx, `
			SELECT staff_id, roles, password_hash, failed_attempts, locked_until, status
			  FROM identity.staff WHERE email = $1 FOR UPDATE`, email).Scan(&m.ID, &m.Roles, &hash, &failed, &lockedUntil, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != "ACTIVE" {
			return domain.ErrBlocked
		}
		if lockedUntil != nil && lockedUntil.After(now) {
			return domain.ErrLocked
		}
		if verify(hash) {
			_, err := tx.Exec(ctx, `UPDATE identity.staff SET failed_attempts = 0, locked_until = NULL WHERE staff_id = $1`, m.ID)
			return err
		}
		failed++
		var lock *time.Time
		if failed >= policy.MaxFailures {
			t := now.Add(policy.LockFor)
			lock, failed = &t, 0
		}
		if _, err := tx.Exec(ctx, `UPDATE identity.staff SET failed_attempts = $2, locked_until = $3 WHERE staff_id = $1`, m.ID, failed, lock); err != nil {
			return err
		}
		outcome = domain.ErrBadCredentials
		return nil
	})
	if err != nil {
		return StaffMember{}, err
	}
	if outcome != nil {
		return StaffMember{}, outcome
	}
	return m, nil
}

// CreateStaff inserts a staff account. Used only by the bootstrap command.
func (s *Store) CreateStaff(ctx context.Context, id uuid.UUID, email, passwordHash string, roles []string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO identity.staff (staff_id, email, password_hash, roles) VALUES ($1,$2,$3,$4)`, id, email, passwordHash, roles)
	if pgxutil.IsUniqueViolation(err, "") {
		return domain.ErrAlreadyRegistered
	}
	return err
}
