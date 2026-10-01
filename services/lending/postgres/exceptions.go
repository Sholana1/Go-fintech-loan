package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/services/lending/domain"
)

// OpenException records a discrepancy. The same (kind, entity) is recorded
// once while open; it returns false if it was already open.
func (q *Queries) OpenException(ctx context.Context, kind, entityType, entityID string, amount int64, detail map[string]any) (bool, error) {
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `
		INSERT INTO lending.recon_exceptions (exception_id, kind, entity_type, entity_id, amount_minor, detail)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (kind, entity_type, entity_id) WHERE state = 'OPEN' DO NOTHING`,
		uuid.New(), kind, entityType, entityID, amount, raw)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ResolveException closes an exception with a note. It returns false if the
// exception was not open.
func (q *Queries) ResolveException(ctx context.Context, id uuid.UUID, by, note string, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
		UPDATE lending.recon_exceptions SET state = 'RESOLVED', resolved_at = $2, resolved_by = $3, resolution_note = $4
		 WHERE exception_id = $1 AND state = 'OPEN'`, id, now, by, note)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ResolveExceptionsFor closes any open exception of a kind for an entity;
// used when the system itself observes that the discrepancy has cleared.
func (q *Queries) ResolveExceptionsFor(ctx context.Context, kind, entityType, entityID, note string, now time.Time) error {
	_, err := q.db.Exec(ctx, `
		UPDATE lending.recon_exceptions SET state = 'RESOLVED', resolved_at = $5, resolved_by = 'system', resolution_note = $4
		 WHERE kind = $1 AND entity_type = $2 AND entity_id = $3 AND state = 'OPEN'`, kind, entityType, entityID, note, now)
	return err
}

// Exceptions lists exceptions in a state, oldest first.
func (q *Queries) Exceptions(ctx context.Context, state string, limit int) ([]domain.ReconException, error) {
	rows, err := q.db.Query(ctx, `
		SELECT exception_id, kind, entity_type, entity_id, amount_minor, detail, state, opened_at, resolved_at,
		       coalesce(resolved_by,''), coalesce(resolution_note,'')
		  FROM lending.recon_exceptions WHERE state = $1 ORDER BY opened_at LIMIT $2`, state, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ReconException, error) {
		var e domain.ReconException
		err := row.Scan(&e.ID, &e.Kind, &e.EntityType, &e.EntityID, &e.AmountMinor, &e.Detail, &e.State, &e.OpenedAt, &e.ResolvedAt, &e.ResolvedBy, &e.ResolutionNote)
		return e, err
	})
}
