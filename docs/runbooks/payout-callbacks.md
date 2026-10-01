# Payout callbacks: duplicate, rejected, contradictory

| Signal | Meaning | Action |
|---|---|---|
| `loan_payout_callbacks_total{outcome="duplicate"}` rising | The provider re-delivers events. Each is acknowledged and has no effect. | None. Expected. |
| `outcome="rejected"` rising | Callbacks failing signature or timestamp checks | Check whether the shared secret was rotated on one side only, or clocks have drifted more than five minutes. If neither, treat as forged traffic: capture source addresses and inform security. No payout was changed. |
| `outcome="unknown_reference"`, exception `PAYOUT_UNKNOWN_REFERENCE` | A callback named a transfer we never created | Confirm with the provider whose transfer it is. If it debited our settlement account, it is a provider dispute. |
| Exception `PAYOUT_PROVIDER_CONFLICT` | The provider reported failure for a payout already captured as successful (or success for one never sent) | Nothing was reversed automatically, by design. Get the provider's final written position. If the transfer truly failed after we captured, the customer is owed a refund: a correcting journal with maker-checker and finance approval. |

## Why duplicates and reordering are safe

Every provider answer, from any source, goes through one function that looks at the payout's current state:

| Payout state | `SUCCESS` | `FAILED` |
|---|---|---|
| `SENDING`, `UNKNOWN` | capture the hold → `SUCCEEDED` | release the hold → `FAILED` |
| `SUCCEEDED`, `SETTLED` | no effect | exception, nothing reversed |
| `FAILED` | late success: debit or exception | no effect |

Capture and release are idempotent in the ledger, and each state change is compare-and-set.

## A lost callback

Is not a problem. Callbacks only speed things up; the status query and the settlement report reach the same result.
