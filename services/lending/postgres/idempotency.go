package postgres

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/services/lending/domain"
)

// ClaimIdempotencyKey records that (principal, endpoint, key) produced
// resourceID. It must run in the same transaction that creates the resource.
//
// If the key already exists (a retry, or a concurrent duplicate that this
// statement waits for), it returns the original resource with claimed=false,
// or ErrIdempotencyMismatch when the request body differs from the original.
func (q *Queries) ClaimIdempotencyKey(ctx context.Context, principal uuid.UUID, endpoint, key string, requestHash []byte, resourceID uuid.UUID, expiresAt time.Time) (uuid.UUID, bool, error) {
	tag, err := q.db.Exec(ctx, `
		INSERT INTO lending.idempotency_keys (principal_id, endpoint, idem_key, request_hash, resource_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT DO NOTHING`, principal, endpoint, key, requestHash, resourceID, expiresAt)
	if err != nil {
		return uuid.Nil, false, err
	}
	if tag.RowsAffected() == 1 {
		return resourceID, true, nil
	}
	var (
		storedHash []byte
		existing   uuid.UUID
	)
	if err := q.db.QueryRow(ctx, `
		SELECT request_hash, resource_id FROM lending.idempotency_keys
		 WHERE principal_id = $1 AND endpoint = $2 AND idem_key = $3`, principal, endpoint, key).Scan(&storedHash, &existing); err != nil {
		return uuid.Nil, false, err
	}
	if !bytes.Equal(storedHash, requestHash) {
		return uuid.Nil, false, domain.ErrIdempotencyMismatch
	}
	return existing, false, nil
}

// LookupIdempotencyKey returns the resource a key produced, if any. It lets
// a use case answer a retry before doing any work.
func (q *Queries) LookupIdempotencyKey(ctx context.Context, principal uuid.UUID, endpoint, key string, requestHash []byte) (uuid.UUID, bool, error) {
	var (
		storedHash []byte
		existing   uuid.UUID
	)
	err := q.db.QueryRow(ctx, `
		SELECT request_hash, resource_id FROM lending.idempotency_keys
		 WHERE principal_id = $1 AND endpoint = $2 AND idem_key = $3`, principal, endpoint, key).Scan(&storedHash, &existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	if !bytes.Equal(storedHash, requestHash) {
		return uuid.Nil, false, domain.ErrIdempotencyMismatch
	}
	return existing, true, nil
}

// DeleteExpiredIdempotencyKeys removes keys past their retention.
func (q *Queries) DeleteExpiredIdempotencyKeys(ctx context.Context, now time.Time) (int64, error) {
	tag, err := q.db.Exec(ctx, `DELETE FROM lending.idempotency_keys WHERE expires_at < $1`, now)
	return tag.RowsAffected(), err
}
