package postgres

import (
	"context"
	"encoding/json"
)

// Actor identifies who did something, for the audit trail.
type Actor struct {
	Kind string // "customer" | "staff" | "system"
	ID   string
}

// SystemActor is the actor for automated steps.
var SystemActor = Actor{Kind: "system", ID: "lending"}

// Audit appends one audit record. detail must not contain personal data
// beyond opaque identifiers.
func (q *Queries) Audit(ctx context.Context, actor Actor, action, entityType, entityID string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `
		INSERT INTO lending.audit_log (actor_kind, actor_id, action, entity_type, entity_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6)`, actor.Kind, actor.ID, action, entityType, entityID, raw)
	return err
}
