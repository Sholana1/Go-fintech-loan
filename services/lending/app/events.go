package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"bankplatform.internal/platform/bizdate"
	"bankplatform.internal/platform/outbox"
	"bankplatform.internal/services/ledger/contract"
	"bankplatform.internal/services/lending/postgres"
)

func endOfBusinessDate(d time.Time) time.Time { return bizdate.StartOf(d.AddDate(0, 0, 1)) }

func sameBusinessDate(t, d time.Time) bool { return bizdate.Of(t).Equal(d) }

// ConsumerCollections is this consumer's name in the inbox table.
const ConsumerCollections = "lending.collections"

// HandleLedgerEvent consumes ledger.journal.posted.
//
// Purpose: when money arrives in the account of a customer who has an
// overdue loan with auto-debit authorised, make that loan due for collection
// now instead of at its next scheduled retry.
//
// Why Kafka: this is a reaction to a committed fact owned by another
// service, it does not need to block the posting that caused it, and other
// consumers will want the same events. Nothing about the customer's balance
// is decided here; the collection itself goes through the ledger.
//
// Delivery is at-least-once. The inbox row and the effect commit together,
// so a redelivered event (after a crash between this commit and the offset
// commit) finds its id in the inbox and does nothing.
func (s *Service) HandleLedgerEvent(ctx context.Context, value []byte) error {
	var env outbox.Envelope
	if err := json.Unmarshal(value, &env); err != nil {
		return fmt.Errorf("decode event envelope: %w", err)
	}
	age := s.now().Sub(env.OccurredAt)
	if env.EventType != contract.EventJournalPosted {
		s.metrics.EventConsumed(env.EventType, "ignored", age)
		return nil
	}
	if env.SchemaVersion != 1 {
		// An unknown major version is not guessed at; it goes to the
		// dead-letter topic for a person to look at.
		return fmt.Errorf("unsupported %s schema version %d", env.EventType, env.SchemaVersion)
	}
	var ev contract.JournalPosted
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		return fmt.Errorf("decode %s: %w", env.EventType, err)
	}

	var customers []uuid.UUID
	for _, l := range ev.Lines {
		if l.Direction != "CREDIT" {
			continue
		}
		// Customer deposit codes are "CUST:<customer id>:MAIN".
		parts := strings.Split(l.AccountCode, ":")
		if len(parts) != 3 || parts[0] != "CUST" {
			continue
		}
		if id, err := uuid.Parse(parts[1]); err == nil {
			customers = append(customers, id)
		}
	}

	outcome := "applied"
	err := s.store.InTx(ctx, func(q *postgres.Queries) error {
		first, err := q.MarkEventProcessed(ctx, ConsumerCollections, env.EventID)
		if err != nil {
			return err
		}
		if !first {
			outcome = "duplicate"
			return nil
		}
		now := s.now()
		for _, c := range customers {
			if _, err := q.WakeCollectionsForCustomer(ctx, c, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.metrics.EventConsumed(env.EventType, outcome, age)
	return nil
}
