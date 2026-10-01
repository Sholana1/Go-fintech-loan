# Database failover and recovery

This runbook describes how the code behaves. The failover mechanism itself (managed PostgreSQL with a synchronous standby, per the architecture plan) is infrastructure that has **not been provisioned or exercised**; see the launch dependencies.

## Failover of a primary (no data loss expected)

What happens in the services:

| Component | Behaviour |
|---|---|
| In-flight transactions | Fail. Nothing partial commits: each money movement is one transaction. |
| API requests | Return 5xx or 503. Clients retry with the **same Idempotency-Key**. |
| Posting intents | Stay `PENDING`; the sweeper resumes. The ledger deduplicates on the intent id. |
| Payouts | A payout caught in `SENDING` is treated as unknown and resolved by status query. Not re-sent. |
| Outbox relay | Loses its advisory lock with its connection; an instance re-acquires it and continues. Events may be published twice; consumers deduplicate. |
| Kafka consumer | Handler errors are retried, then dead-lettered; offsets are not committed for unprocessed records. |

Tested locally by terminating database backends mid-transaction (`TestInterruptedTransactionsLeaveNoPartialPostings`, `TestCrashAfterPublishRepublishesWithTheSameEventID`).

After failover:

1. `ledgerd verify` on the ledger.
2. Watch `loan_posting_intent_oldest_pending_seconds` return to zero.
3. Check `loan_recon_exceptions_open`.

## Point-in-time restore, or promotion of an asynchronous replica (data loss possible)

Committed transactions after the recovery point are gone from our databases but **may have happened in the outside world**.

1. **Do not reopen for traffic yet.**
2. Restore ledger, identity and lending to the **same** point in time. They are separate databases; a restore that leaves lending ahead of the ledger produces `JOURNAL_MISMATCH` for every posting in the gap.
3. Run `ledgerd verify`.
4. Run payout reconciliation for every business date from the day before the recovery point. For each transfer the provider has and we do not: it is an `UNKNOWN_REFERENCE` exception. Each is a payment that left the bank and must be re-established in the books by an approved journal.
5. Run ledger reconciliation. Resolve every exception before reopening.
6. Kafka may hold events for transactions that no longer exist. Consumers must tolerate an event whose aggregate is missing. (Lending's one consumer does: it only schedules a collection.)
7. Customers whose loans were disbursed in the lost window received money the books no longer show, or had repayments debited that the books no longer show. These are found in step 5 as control-account differences and must be rebuilt case by case.

**Loss exposure** is bounded by the replication lag at the moment of failure and discovered by reconciliation against the provider. This is why reconciliation is a first-class part of the product and not a report.

## Before launch

Each row of the plan's RTO/RPO table needs an exercise with evidence. None has been run.
