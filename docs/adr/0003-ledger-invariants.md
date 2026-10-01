# 0003: Which mechanism enforces each ledger invariant

| Invariant | Enforced by | Where | Test |
|---|---|---|---|
| Every journal balances per currency | Deferred constraint trigger at COMMIT. A row-level CHECK cannot do this: it sees one row, and balance is a property of the set | `00001_ledger_core.sql` `entries_balanced` | `TestDatabaseRejectsUnbalancedJournalAtCommit` |
| Request is rejected early with a clear error | `domain.ValidateLines` | `services/ledger/domain` | `TestValidateLines` |
| An operation is posted once | Primary key on `posting_refs (op_type, op_id, op_step)`; claimed first in the transaction; a concurrent duplicate waits on the index | `postgres.claimRef` | `TestSameReferencePostsExactlyOnce` |
| A reference cannot be reused for different content | Fingerprint stored with the reference | `domain.Fingerprint` | `TestReferenceReusedWithDifferentContentIsRejected` |
| Posted rows are immutable | Triggers reject UPDATE, DELETE, TRUNCATE; the application role has no such grant | migration | `TestPostedRowsAreImmutableAndBalancesAreNotWritable` |
| Balances equal the sum of entries | Balances are written only by a trigger on entry insert; the application role cannot update them | `ledger.apply_entry` | verifier in every integration test |
| Concurrent spending cannot exceed funds | Row lock on the balance row, check under the lock, and `CHECK (posted - held >= floor)` as backstop | `postgres.post` | `TestConcurrentDebitsNeverOverspend` |
| No deadlock between opposing postings | Balance rows locked in one global order | `postgres.lockAccounts` | `TestOpposingPostingsDoNotDeadlock` |
| Hold and capture are atomic | One transaction closes the hold and posts | `postgres.CaptureHold` | `TestHoldLifecycle`, `TestCaptureAndReleaseRace` |
| A caller can post only what its journal types allow | `posting_rights` and `journal_types` (max customer accounts, allowed system accounts) | `app.checkShape` | `TestAuthorisationIsByWorkloadAndJournalShape` |
| An event exists iff its journal committed | Outbox row in the posting transaction | `postgres.post` | `TestOutboxRowIsWrittenAtomicallyWithTheJournal` |
| Cached or replicated balances never authorise | There is no such path: authorisation happens only inside the posting transaction; `GetBalance` is documented as display-only | — | by construction |

Isolation is READ COMMITTED with explicit locks: the conflict set is known up front, so contention produces waits, not serialization failures.

**Departure from the plan's DDL.** `journal_types` (per-type shape policy) is new. The plan said "lending cannot post a card capture"; a right per journal type alone would still let a service bend an allowed type into moving money between two customers.
