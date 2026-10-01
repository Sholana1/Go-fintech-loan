# Ledger gRPC contract (`bank.ledger.v1`)

Definition: [proto/bank/ledger/v1/ledger.proto](../../proto/bank/ledger/v1/ledger.proto). Client-side names: [services/ledger/contract](../../services/ledger/contract/contract.go).

## Per-RPC contract

| RPC | Purpose | Authorisation (workload must hold) | Idempotent on | Typical deadline |
|---|---|---|---|---|
| `OpenAccount` | Create a customer deposit account | `OPEN_ACCOUNT` | `code` | 2 s |
| `PostJournal` | Check funds and post a balanced journal atomically | the journal type; lines must fit that type's shape policy | `ref` | 2 s |
| `PlaceHold` | Reserve available funds | `HOLD:<op_type>` | `ref` | 2 s |
| `CaptureHold` | Close a hold and post the journal that consumes it | `HOLD:<op_type>` and the journal type | `ref` | 2 s |
| `ReleaseHold` | Close a hold without posting | `HOLD:<op_type>` | hold `ref` | 2 s |
| `GetBalance` | Read a balance (never an authorisation to spend) | `READ` | — | 1 s |
| `GetJournal` | Read a journal by reference | `READ` | — | 1 s |

Input validation: at least two lines, at most 32; amounts strictly positive; debits equal credits per currency; account codes well-formed; business date not in the future.

## Status mapping

| gRPC status | Reasons (`google.rpc.ErrorInfo.reason`) | Client action |
|---|---|---|
| `INVALID_ARGUMENT` | `INVALID_REQUEST`, `JOURNAL_UNBALANCED`, `CURRENCY_MISMATCH` | Fix the request. Never retry. |
| `PERMISSION_DENIED` | `NOT_AUTHORISED` | Never retry. |
| `NOT_FOUND` | `ACCOUNT_NOT_FOUND`, `HOLD_NOT_FOUND`, `JOURNAL_NOT_FOUND` | Depends on the caller's flow. |
| `FAILED_PRECONDITION` | `INSUFFICIENT_FUNDS`, `ACCOUNT_NOT_ACTIVE`, `POSTING_REF_REUSED`, `HOLD_NOT_ACTIVE`, `HOLD_ALREADY_CAPTURED`, `ACCOUNT_CONFLICT` | A business outcome. Do not retry unchanged. |
| `ABORTED` | — | Lock contention after bounded internal retries. Retry with the **same** reference. |
| `RESOURCE_EXHAUSTED` | — | Server at its concurrency limit. Retry with the same reference, with backoff. |
| `UNAVAILABLE`, `DEADLINE_EXCEEDED` | — | Outcome **unknown**. Retry with the same reference to learn it. |
| `UNAUTHENTICATED` | — | No verified client certificate. |
| `INTERNAL` | — | Logged server-side; details are not returned. |

**The one rule clients must follow:** never retry a mutating call with a new reference. A new reference is a new operation.

## Compatibility

Field numbers are never reused; removed fields are `reserved`. New fields are optional and have safe zero values. Enum zero values are `*_UNSPECIFIED`. `buf breaking` (wire and JSON) runs in CI against the main branch.

## Event

Topic `ledger.journal.v1`, type `ledger.journal.posted`, schema version 1, key = operation id. Payload: `contract.JournalPosted` (journal id and type, business date, posting reference, lines with account code, direction, amount, currency). No personal data.
