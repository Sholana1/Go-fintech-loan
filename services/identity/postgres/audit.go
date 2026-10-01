package postgres

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Audit writes one audit record outside any other transaction.
func (s *Store) Audit(ctx context.Context, actor, action string, subject *uuid.UUID, detail map[string]any) error {
	return s.tx.InTx(ctx, func(tx pgx.Tx) error { return audit(ctx, tx, actor, action, subject, detail) })
}

func audit(ctx context.Context, tx pgx.Tx, actor, action string, subject *uuid.UUID, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity.audit_log (actor, action, subject_id, detail) VALUES ($1,$2,$3,$4)`, actor, action, subject, raw)
	return err
}
