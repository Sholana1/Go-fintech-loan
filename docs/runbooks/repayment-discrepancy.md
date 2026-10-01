# Repayment discrepancy

## Symptoms

- Customer: "I was debited but my loan still shows the instalment unpaid."
- Alert `LoanPostingIntentPendingTooLong`.
- Exception `JOURNAL_MISMATCH` or `CONTROL_ACCOUNT_MISMATCH`.

## Diagnose

```sql
-- lending
SELECT r.repayment_id, r.state, r.applied_minor, r.unapplied_minor, r.reject_reason, r.journal_id,
       i.state AS intent_state, i.attempts, i.next_attempt_at
  FROM lending.repayments r JOIN lending.posting_intents i USING (intent_id)
 WHERE r.loan_id = '<loan id>' ORDER BY r.created_at;

-- ledger: op_id is the intent id
SELECT journal_id, posted_at FROM ledger.journals
 WHERE op_type IN ('LENDING_REPAYMENT','LENDING_RECOVERY') AND op_id = '<intent id>';
```

| Repayment | Intent | Journal in ledger | Meaning | Action |
|---|---|---|---|---|
| `PENDING` | `PENDING` | yes | Debited; lending has not applied it yet | The sweeper applies it, once. If stuck, look at lending's logs. Do not refund. |
| `PENDING` | `PENDING` | no | Not debited yet | The sweeper retries. Other repayments on this loan get `OPERATION_IN_PROGRESS` until it completes. |
| `REJECTED` | `REJECTED` | no | The ledger refused (usually insufficient funds); nothing moved | Explain to the customer. |
| `ALLOCATED` | `POSTED` | yes | Normal | Check the allocation: fees first, then interest, then principal, oldest first. The customer may have expected principal to fall when a fee or interest was paid. |
| `ALLOCATED` | `POSTED` | **no** | Should be impossible | Stop. Raise an incident. See [ledger-discrepancy.md](ledger-discrepancy.md). |

## "You took less than I sent"

By design. Only what the product rules can apply is debited; the rest stays in the customer's account and is shown as `unapplied_minor`. Before a due date, a payment covers the next instalment only unless it settles the whole loan.

## Never

- Edit `lending.instalments`. Reconciliation will flag it within five minutes, and it will be wrong against the ledger.
- Refund a debit whose repayment is still `PENDING`.
