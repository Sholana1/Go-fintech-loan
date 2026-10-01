# External repayment: paid but not credited, or credited but not applied

A customer pays towards a loan through the payment provider. See [ADR 0011](../adr/0011-external-repayments.md) for the design.

## What the customer sees, and what it means

| API status | Internal state | Meaning |
|---|---|---|
| `AWAITING_PAYMENT` | `INITIATED` | We have not yet got a `SUCCESS` from the provider. Either the customer has not paid, or we cannot reach the provider. |
| `PROCESSING` | `VERIFIED` | The provider confirmed the money. The ledger credit has not been recorded yet. |
| `PROCESSING` | `CREDITED` | The customer's deposit account was credited. The repayment has not been applied yet. |
| `PROCESSING` | `REVIEW` | The provider confirmed something that cannot be credited by rule (wrong currency). |
| `APPLIED` | `APPLIED` | Done. |
| `CREDITED_TO_ACCOUNT` | `UNAPPLIED` | The money is in the customer's deposit account; the loan did not need it. |
| `FAILED` / `EXPIRED` | same | The provider said failed, or nothing was paid in 30 minutes. Either becomes `APPLIED` if the provider later proves the money arrived. |

## "I paid but my loan was not credited"

1. Find the payment: `SELECT payment_id, state, last_code, attempts, next_attempt_at, verified_minor FROM lending.external_payments WHERE reference = '<rp-…>'`.
2. `INITIATED` with `last_code = VERIFY_UNAVAILABLE`: we cannot get an answer from the provider. Check `loan_provider_call_seconds_count{operation="verify_payment",outcome="error"}` and the provider's status page. **Do not credit manually on the customer's receipt.** When the provider answers, the payment completes without action. After 30 minutes an `EXTERNAL_PAYMENT_UNRESOLVED` exception is open for it.
3. `INITIATED` with `last_code = AWAITING_PAYMENT`: the provider has no successful payment under this reference. Ask the customer for the reference on their receipt; if it differs, the payment was made outside this flow and is an `EXTERNAL_PAYMENT_UNKNOWN_REFERENCE` case (below).
4. `EXPIRED` or `FAILED` and the customer has proof of payment: confirm with the provider that the reference shows success, then have the webhook re-sent (or wait for the provider's own retry). A signed webhook puts the payment back on the queue; the system verifies and credits it. There is no manual credit path.
5. `VERIFIED` for more than a few minutes (alert `LoanExternalPaymentNotCredited`): the ledger is unreachable or refusing. `last_code` says which. `LEDGER_REJECTED` also opens a `POSTING_REJECTED` exception: the customer's account is closed or frozen, and a person must decide where the money goes.
6. `CREDITED` for more than a few minutes: another posting on the loan is in flight (`LOAN_BUSY`) or the repayment posting is pending (`REPAYMENT_PENDING`). See [failed-or-stuck-disbursement.md](failed-or-stuck-disbursement.md) for stuck posting intents. The customer already has the money in their account.

## Exceptions

| Kind | Meaning | Action |
|---|---|---|
| `EXTERNAL_PAYMENT_UNKNOWN_REFERENCE` | A webhook named a reference we never issued | Money may have arrived that we cannot attribute. Identify the payer with the provider; refund through the provider or credit through a maker-checker correcting journal with finance approval. |
| `EXTERNAL_PAYMENT_CURRENCY_MISMATCH` | Confirmed in a currency other than the loan's | Nothing was credited. Refund through the provider, or agree a conversion with finance. |
| `EXTERNAL_PAYMENT_UNRESOLVED` | No answer from the provider for longer than the validity window | Provider incident. The poller keeps asking every 5 minutes. |
| `CONTROL_ACCOUNT_MISMATCH` on `SYS:COLLECTIONS_CLEARING` | The ledger's clearing balance differs from the sum of confirmed payments | See [ledger-discrepancy.md](ledger-discrepancy.md). Until provider settlement postings exist, any posting to this account other than a payment credit is a break. |

## Why duplicates, late webhooks and crashes are safe

- A webhook never says what happened; it only makes us ask the provider. A forged or replayed webhook costs one status call.
- Every state change is compare-and-set, so the webhook and the poller cannot both act on the same step.
- The ledger credit is posted under the payment id: posting it twice yields one journal.
- The repayment's id is the payment's id: it cannot be created twice.
