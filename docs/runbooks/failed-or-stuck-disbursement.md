# Disbursement failed or stuck

## Symptoms

- Alert `LoanDisbursementPendingTooLong` (`loan_disbursement_oldest_pending_seconds` above 60 s).
- Customer reports "accepted the loan, no money".
- Open exception of kind `POSTING_REJECTED` on a loan.

## What the states mean

| Loan state | Meaning | Money moved? |
|---|---|---|
| `PENDING_DISBURSEMENT` | Accepted; the posting has not been confirmed | Possibly: check the ledger |
| `ACTIVE` | The ledger posted the disbursement | Yes |
| `DISBURSEMENT_FAILED` | The ledger refused it | No |

## Diagnose

```sql
-- lending database
SELECT i.intent_id, i.state, i.attempts, i.next_attempt_at, i.reject_reason, l.state AS loan_state
  FROM lending.posting_intents i JOIN lending.loans l USING (loan_id)
 WHERE i.kind = 'DISBURSEMENT' AND l.loan_id = '<loan id>';

-- ledger database: did it post? (op_id is the intent id)
SELECT journal_id, posted_at FROM ledger.journals
 WHERE op_type = 'LENDING_DISBURSEMENT' AND op_id = '<intent id>';
```

## Act

| Finding | Action |
|---|---|
| Intent `PENDING`, ledger healthy | Nothing: the posting sweeper retries every 2 s with the same reference. If it is not progressing, check lending's logs for the ledger error and `ledger_postings_rejected_total`. |
| Intent `PENDING`, ledger unavailable | Restore the ledger. Do **not** create a new loan or offer; the pending intent completes by itself and cannot post twice. |
| Intent `PENDING`, journal already in the ledger | Nothing: the next sweep receives "already posted" and marks the loan `ACTIVE`. `loan_duplicate_postings_prevented_total` increments. |
| Intent `REJECTED`, loan `DISBURSEMENT_FAILED` | The customer owes nothing and received nothing. Read `reject_reason`. If the customer's account is frozen or restricted, that is a compliance matter, not a lending fix. Tell the customer the loan did not complete. They may apply again; the failed loan does not block a new application. Resolve the exception with a note. |

## Never

- Update `lending.loans.state` by hand. The table refuses `ACTIVE` without a journal.
- Post a manual credit to "make the customer whole" while an intent is still pending.
