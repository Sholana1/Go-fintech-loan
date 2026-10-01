package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/lending/domain"
)

const adminActionColumns = `action_id, kind, loan_id, params, reason, state, maker_id, checker_id,
	coalesce(decision_note,''), created_at, decided_at, executed_at`

func scanAdminAction(row pgx.Row) (domain.AdminAction, error) {
	var (
		a           domain.AdminAction
		kind, state string
	)
	err := row.Scan(&a.ID, &kind, &a.LoanID, &a.Params, &a.Reason, &state, &a.MakerID, &a.CheckerID,
		&a.DecisionNote, &a.CreatedAt, &a.DecidedAt, &a.ExecutedAt)
	a.Kind, a.State = domain.AdminActionKind(kind), domain.AdminActionState(state)
	return a, err
}

// InsertAdminAction stores a proposed action. One open action per loan.
func (q *Queries) InsertAdminAction(ctx context.Context, a domain.AdminAction) error {
	_, err := q.db.Exec(ctx, `
		INSERT INTO lending.admin_actions (action_id, kind, loan_id, params, reason, state, maker_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, a.ID, string(a.Kind), a.LoanID, a.Params, a.Reason, string(a.State), a.MakerID, a.CreatedAt)
	if pgxutil.IsUniqueViolation(err, "one_open_admin_action") {
		return domain.ErrConflict
	}
	return err
}

func (q *Queries) LockAdminAction(ctx context.Context, id uuid.UUID) (domain.AdminAction, error) {
	a, err := scanAdminAction(q.db.QueryRow(ctx, `SELECT `+adminActionColumns+` FROM lending.admin_actions WHERE action_id = $1 FOR UPDATE`, id))
	return a, notFound(err, "admin action")
}

func (q *Queries) GetAdminAction(ctx context.Context, id uuid.UUID) (domain.AdminAction, error) {
	a, err := scanAdminAction(q.db.QueryRow(ctx, `SELECT `+adminActionColumns+` FROM lending.admin_actions WHERE action_id = $1`, id))
	return a, notFound(err, "admin action")
}

func (q *Queries) AdminActionByIntent(ctx context.Context, intentID uuid.UUID) (domain.AdminAction, error) {
	a, err := scanAdminAction(q.db.QueryRow(ctx, `SELECT `+adminActionColumns+` FROM lending.admin_actions WHERE intent_id = $1`, intentID))
	return a, notFound(err, "admin action")
}

// DecideAdminAction records the checker's decision. The table's CHECK
// constraint (checker_id <> maker_id) is the database-level guarantee of
// maker-checker; the use case also checks it to return a clear error.
func (q *Queries) DecideAdminAction(ctx context.Context, id uuid.UUID, to domain.AdminActionState, checker uuid.UUID, note string, intentID *uuid.UUID, now time.Time) error {
	_, err := q.db.Exec(ctx, `
		UPDATE lending.admin_actions
		   SET state = $2, checker_id = $3, decision_note = nullif($4,''), decided_at = $5, intent_id = $6
		 WHERE action_id = $1 AND state = 'PROPOSED'`, id, string(to), checker, note, now, intentID)
	if pgxutil.Code(err) == pgxutil.CodeCheckViolation {
		return domain.ErrForbidden
	}
	return err
}

func (q *Queries) FinishAdminAction(ctx context.Context, id uuid.UUID, to domain.AdminActionState, now time.Time) error {
	_, err := q.db.Exec(ctx, `
		UPDATE lending.admin_actions SET state = $2, executed_at = $3 WHERE action_id = $1 AND state = 'APPROVED'`, id, string(to), now)
	return err
}

// OpenAdminActions lists actions awaiting a checker.
func (q *Queries) OpenAdminActions(ctx context.Context, limit int) ([]domain.AdminAction, error) {
	rows, err := q.db.Query(ctx, `SELECT `+adminActionColumns+` FROM lending.admin_actions WHERE state = 'PROPOSED' ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AdminAction
	for rows.Next() {
		a, err := scanAdminAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
