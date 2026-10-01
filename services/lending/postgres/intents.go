package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/pgxutil"
	"bankplatform.internal/services/lending/domain"
)

const intentColumns = `intent_id, kind, loan_id, journal_type, lines, business_date, payload, state,
	coalesce(reject_reason,''), journal_id, attempts, created_at`

func scanIntent(row pgx.Row) (domain.PostingIntent, error) {
	var (
		i           domain.PostingIntent
		kind, state string
		lines       []byte
	)
	err := row.Scan(&i.ID, &kind, &i.LoanID, &i.JournalType, &lines, &i.BusinessDate, &i.Payload, &state,
		&i.RejectReason, &i.JournalID, &i.Attempts, &i.CreatedAt)
	if err != nil {
		return i, err
	}
	i.Kind, i.State = domain.IntentKind(kind), domain.IntentState(state)
	return i, json.Unmarshal(lines, &i.Lines)
}

// InsertIntent stores a pending posting intent in the caller's transaction.
//
// For a loan-level intent the partial unique index allows only one PENDING
// intent per loan. If another is in flight the insert fails and
// ErrOperationInProgress is returned; the caller's whole transaction rolls
// back and the operation can be retried shortly.
func (q *Queries) InsertIntent(ctx context.Context, i domain.PostingIntent, nextAttemptAt time.Time) error {
	lines, err := json.Marshal(i.Lines)
	if err != nil {
		return err
	}
	payload := i.Payload
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	_, err = q.db.Exec(ctx, `
		INSERT INTO lending.posting_intents (intent_id, kind, loan_id, journal_type, lines, business_date, payload, next_attempt_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		i.ID, string(i.Kind), i.LoanID, i.JournalType, lines, i.BusinessDate, payload, nextAttemptAt)
	if pgxutil.IsUniqueViolation(err, "one_pending_intent_per_loan") {
		return domain.ErrOperationInProgress
	}
	return err
}

// HasPendingIntent reports whether a posting is in flight for the loan.
func (q *Queries) HasPendingIntent(ctx context.Context, loanID uuid.UUID) (bool, error) {
	var pending bool
	err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lending.posting_intents WHERE loan_id = $1 AND state = 'PENDING')`, loanID).Scan(&pending)
	return pending, err
}

func (q *Queries) GetIntent(ctx context.Context, id uuid.UUID) (domain.PostingIntent, error) {
	i, err := scanIntent(q.db.QueryRow(ctx, `SELECT `+intentColumns+` FROM lending.posting_intents WHERE intent_id = $1`, id))
	return i, notFound(err, "posting intent")
}

// LockIntent reads the intent FOR UPDATE. The apply step takes this lock
// first, so two workers that both posted the same intent to the ledger
// (harmless: the ledger deduplicates) cannot both apply its effect.
func (q *Queries) LockIntent(ctx context.Context, id uuid.UUID) (domain.PostingIntent, error) {
	i, err := scanIntent(q.db.QueryRow(ctx, `SELECT `+intentColumns+` FROM lending.posting_intents WHERE intent_id = $1 FOR UPDATE`, id))
	return i, notFound(err, "posting intent")
}

func (q *Queries) MarkIntentPosted(ctx context.Context, id uuid.UUID, journalID int64, now time.Time) error {
	_, err := q.db.Exec(ctx, `
		UPDATE lending.posting_intents SET state = 'POSTED', journal_id = $2, completed_at = $3
		 WHERE intent_id = $1 AND state = 'PENDING'`, id, journalID, now)
	return err
}

func (q *Queries) MarkIntentRejected(ctx context.Context, id uuid.UUID, reason string, now time.Time) error {
	_, err := q.db.Exec(ctx, `
		UPDATE lending.posting_intents SET state = 'REJECTED', reject_reason = $2, completed_at = $3
		 WHERE intent_id = $1 AND state = 'PENDING'`, id, reason, now)
	return err
}

// DeferIntent records a failed attempt and schedules the next one.
func (q *Queries) DeferIntent(ctx context.Context, id uuid.UUID, nextAttemptAt time.Time) error {
	_, err := q.db.Exec(ctx, `
		UPDATE lending.posting_intents SET attempts = attempts + 1, next_attempt_at = $2
		 WHERE intent_id = $1 AND state = 'PENDING'`, id, nextAttemptAt)
	return err
}

// DueIntents lists pending intents whose next attempt is due.
func (q *Queries) DueIntents(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := q.db.Query(ctx, `
		SELECT intent_id FROM lending.posting_intents
		 WHERE state = 'PENDING' AND next_attempt_at <= $1
		 ORDER BY next_attempt_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// RecentPostedIntents lists intents posted since `since`.
func (q *Queries) RecentPostedIntents(ctx context.Context, since time.Time, limit int) ([]domain.PostingIntent, error) {
	rows, err := q.db.Query(ctx, `
		SELECT `+intentColumns+` FROM lending.posting_intents
		 WHERE state = 'POSTED' AND completed_at >= $1 ORDER BY completed_at LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PostingIntent
	for rows.Next() {
		i, err := scanIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
