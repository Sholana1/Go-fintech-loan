# Events

Envelope (every event, JSON):

```json
{
  "event_id": "uuid",            "event_type": "loan.disbursed",
  "schema_version": 1,           "aggregate_type": "loan",
  "aggregate_id": "uuid",        "aggregate_version": 3,
  "occurred_at": "RFC 3339",     "traceparent": "W3C trace context",
  "data": { }
}
```

Kafka headers repeat `event_id`, `event_type`, `schema_version`, `traceparent`.

| Topic | Key (ordering scope) | Producer | Types |
|---|---|---|---|
| `ledger.journal.v1` | operation id | ledger | `ledger.journal.posted` |
| `lending.loan.v1` | application id: one application and its loan are ordered | lending | `loan.application.submitted`, `.offered`, `.declined`, `.referred`, `.expired`, `.cancelled`; `loan.offer.accepted`; `loan.disbursed`, `loan.disbursement_failed`; `loan.repayment.allocated`; `loan.closed`; `loan.fee.charged`; `loan.arrears.changed`; `loan.restructured`; `loan.written_off`; `loan.recovery.received`; `loan.payout.succeeded`, `.failed`, `.settled`; `loan.external_payment.credited`, `.applied`, `.unapplied`, `.failed` |

Rules:

- **Delivery is at-least-once.** Deduplicate on `event_id`, committing the deduplication record with the effect.
- **Ordering** holds per key only.
- **Compatibility.** Within a schema version fields are only added; consumers ignore unknown fields. A breaking change is a new `schema_version`; a consumer that meets a version it does not know fails the record to its dead-letter topic.
- **Privacy.** Payloads carry opaque ids, states, reason codes and amounts. No names, phone numbers, BVNs, account numbers or bureau data. This is tested.
