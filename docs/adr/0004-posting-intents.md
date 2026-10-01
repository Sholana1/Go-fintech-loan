# 0004: Posting intents for every lending→ledger movement

**Problem.** Lending and the ledger are separate databases. A loan operation must change both, and there is no transaction across them.

**Decision.** Every movement is a *posting intent*: a row written in the same lending transaction as the business change that needs it. Its id is the ledger posting reference.

```
tx A (lending)  business rows + intent(PENDING)             commit
call            ledger.PostJournal(ref = intent id)          idempotent
tx B (lending)  intent -> POSTED|REJECTED + apply effect     commit
```

A sweeper re-drives pending intents. Because the ledger deduplicates on the reference, repeating the call after any crash yields one journal.

**One pending intent per loan** (partial unique index). A repayment's allocation is computed against the schedule in tx A and applied in tx B; nothing else may change that schedule in between. A second operation on the same loan gets `OPERATION_IN_PROGRESS` and retries.

**Lock order** is loan, then intent, everywhere.

**Alternatives rejected.**
- *Post first, then write lending.* A crash leaves money moved with no record of why.
- *Write lending, publish an event, let the ledger consume it.* Asynchronous disbursement; the customer cannot be told "done" in the request; and the ledger would need to know about loans.
- *A workflow engine.* Another stateful system to operate, for a two-step saga.

**Tests.** `TestDisbursementSurvivesLedgerOutageAtAcceptance`, `TestDisbursementIsNotRepeatedAfterALostLedgerResponse`, `TestDuplicateDisbursementCommandsPostOnce`, `TestRepaymentIsAppliedOnceAfterALostLedgerResponse`, `TestConcurrentRepaymentsNeverOverAllocate`.
