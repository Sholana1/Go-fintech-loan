// Package outbox implements the transactional outbox.
//
// Responsibility: guarantee that a domain event is published if and only if
// the database transaction that produced it committed.
//
// How: Insert writes the event row inside the caller's transaction. A Relay
// (one active instance per database, enforced with an advisory lock) reads
// unpublished rows, publishes them durably, then marks them published.
//
// Consistency boundary: the event row and the business change share one
// PostgreSQL transaction. Publication is at-least-once: if the process dies
// after the broker acknowledged but before the row was marked, the event is
// sent again with the same event_id. Consumers deduplicate on event_id.
//
// Ordering: rows are published in outbox_id order within one relay batch and
// events of one aggregate share a partition key. Two events of the same
// aggregate are always written by transactions that serialise on that
// aggregate's row lock, so the later event cannot become visible first.
//
// Failure modes: broker unavailable -> rows accumulate and are published when
// it returns (watch the oldest-unpublished-age metric). Relay crash -> the
// advisory lock is released with the session and another instance takes over.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"bankplatform.internal/platform/obs"
)

// Event is a domain event to be published after commit.
type Event struct {
	ID               uuid.UUID // generated if zero
	Topic            string
	PartitionKey     string // ordering scope: events with the same key are ordered
	Type             string // e.g. "loan.disbursed"
	SchemaVersion    int
	AggregateType    string
	AggregateID      string
	AggregateVersion int64
	// Data must be JSON-serialisable and must not contain personal data
	// beyond opaque identifiers; events are retained and widely readable.
	Data any
}

// Envelope is the wire format of every event on Kafka.
type Envelope struct {
	EventID          uuid.UUID       `json:"event_id"`
	EventType        string          `json:"event_type"`
	SchemaVersion    int             `json:"schema_version"`
	AggregateType    string          `json:"aggregate_type"`
	AggregateID      string          `json:"aggregate_id"`
	AggregateVersion int64           `json:"aggregate_version"`
	OccurredAt       time.Time       `json:"occurred_at"`
	TraceParent      string          `json:"traceparent,omitempty"`
	Data             json.RawMessage `json:"data"`
}

var tableName = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)

// Insert writes the event in tx. table is the schema-qualified outbox table
// of the calling service (a compile-time constant, validated defensively).
func Insert(ctx context.Context, tx pgx.Tx, table string, e Event) (uuid.UUID, error) {
	if !tableName.MatchString(table) {
		return uuid.Nil, fmt.Errorf("outbox: invalid table name %q", table)
	}
	if e.Topic == "" || e.Type == "" || e.PartitionKey == "" || e.SchemaVersion <= 0 {
		return uuid.Nil, errors.New("outbox: topic, type, partition key and schema version are required")
	}
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	payload, err := json.Marshal(e.Data)
	if err != nil {
		return uuid.Nil, fmt.Errorf("outbox: encode event data: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO `+table+` (event_id, topic, partition_key, event_type, schema_version,
		                       aggregate_type, aggregate_id, aggregate_version, payload, traceparent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.ID, e.Topic, e.PartitionKey, e.Type, e.SchemaVersion,
		e.AggregateType, e.AggregateID, e.AggregateVersion, payload, obs.TraceParent(ctx))
	if err != nil {
		return uuid.Nil, fmt.Errorf("outbox: insert event: %w", err)
	}
	return e.ID, nil
}
