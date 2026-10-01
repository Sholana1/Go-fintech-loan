package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Message is what the relay hands to a Publisher.
type Message struct {
	Topic   string
	Key     string
	Value   []byte
	Headers map[string]string
}

// Publisher sends messages durably. Publish must return nil only when every
// message has been acknowledged by the broker with full durability.
type Publisher interface {
	Publish(ctx context.Context, msgs []Message) error
}

// Relay publishes unpublished outbox rows.
type Relay struct {
	Pool      *pgxpool.Pool
	Table     string
	Publisher Publisher
	Logger    *slog.Logger
	// LockID is the advisory lock key that makes this relay single-active
	// per database. Each service uses its own constant.
	LockID    int64
	BatchSize int           // default 200
	Interval  time.Duration // poll interval when idle; default 200ms
	Retention time.Duration // published rows older than this are deleted; default 7 days
	// OnBatch is called after each successful batch; for metrics.
	OnBatch func(published int)
}

// Run blocks until ctx is cancelled. It returns nil on cancellation.
func (r *Relay) Run(ctx context.Context) error {
	if !tableName.MatchString(r.Table) {
		return fmt.Errorf("outbox: invalid table name %q", r.Table)
	}
	if r.BatchSize <= 0 {
		r.BatchSize = 200
	}
	if r.Interval <= 0 {
		r.Interval = 200 * time.Millisecond
	}
	if r.Retention <= 0 {
		r.Retention = 7 * 24 * time.Hour
	}
	for {
		err := r.runAsLeader(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			r.Logger.ErrorContext(ctx, "outbox relay stopped; will retry", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// runAsLeader holds one connection for the advisory lock for as long as it
// is the active relay. Losing the connection loses the lock, which is the
// desired failure behaviour.
func (r *Relay) runAsLeader(ctx context.Context) error {
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, r.LockID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil // another instance is the relay; try again later
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, r.LockID)
	}()

	lastCleanup := time.Time{}
	for {
		n, err := r.DrainOnce(ctx)
		if err != nil {
			return err
		}
		if time.Since(lastCleanup) > time.Hour {
			if _, err := r.Pool.Exec(ctx, `DELETE FROM `+r.Table+` WHERE published_at < now() - $1::interval`,
				fmt.Sprintf("%d seconds", int64(r.Retention.Seconds()))); err != nil {
				r.Logger.WarnContext(ctx, "outbox cleanup failed", "error", err.Error())
			}
			lastCleanup = time.Now()
		}
		if n == r.BatchSize {
			continue // more may be waiting
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.Interval):
		}
		// Verify we still hold the session (and therefore the lock).
		if err := conn.Ping(ctx); err != nil {
			return fmt.Errorf("lost leader connection: %w", err)
		}
	}
}

// DrainOnce publishes one batch and returns how many rows it published. It is
// exported so tests can drive the relay deterministically.
func (r *Relay) DrainOnce(ctx context.Context) (int, error) {
	batch := r.BatchSize
	if batch <= 0 {
		batch = 200
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rbCtx) // no-op after commit
	}()

	rows, err := tx.Query(ctx, `
		SELECT outbox_id, event_id, topic, partition_key, event_type, schema_version,
		       aggregate_type, aggregate_id, aggregate_version, payload, coalesce(traceparent,''), occurred_at
		  FROM `+r.Table+`
		 WHERE published_at IS NULL
		 ORDER BY outbox_id
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, batch)
	if err != nil {
		return 0, err
	}
	var ids []int64
	var msgs []Message
	for rows.Next() {
		var (
			id      int64
			env     Envelope
			topic   string
			key     string
			payload []byte
		)
		if err := rows.Scan(&id, &env.EventID, &topic, &key, &env.EventType, &env.SchemaVersion,
			&env.AggregateType, &env.AggregateID, &env.AggregateVersion, &payload, &env.TraceParent, &env.OccurredAt); err != nil {
			rows.Close()
			return 0, err
		}
		env.Data = payload
		value, err := json.Marshal(env)
		if err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		msgs = append(msgs, Message{
			Topic: topic, Key: key, Value: value,
			Headers: map[string]string{
				"event_id":       env.EventID.String(),
				"event_type":     env.EventType,
				"schema_version": fmt.Sprintf("%d", env.SchemaVersion),
				"traceparent":    env.TraceParent,
			},
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	// Publish before marking. If the process dies between the two, the rows
	// stay unpublished and are sent again: at-least-once, never lost.
	if err := r.Publisher.Publish(ctx, msgs); err != nil {
		return 0, fmt.Errorf("publish outbox batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE `+r.Table+` SET published_at = now() WHERE outbox_id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if r.OnBatch != nil {
		r.OnBatch(len(ids))
	}
	return len(ids), nil
}

// OldestUnpublishedAge returns how long the oldest unpublished event has been
// waiting; zero when the outbox is empty. Exported for the outbox-age metric.
func OldestUnpublishedAge(ctx context.Context, pool *pgxpool.Pool, table string) (time.Duration, error) {
	if !tableName.MatchString(table) {
		return 0, fmt.Errorf("outbox: invalid table name %q", table)
	}
	var seconds float64
	err := pool.QueryRow(ctx, `
		SELECT coalesce(extract(epoch FROM now() - min(occurred_at)), 0)
		  FROM `+table+` WHERE published_at IS NULL`).Scan(&seconds)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
