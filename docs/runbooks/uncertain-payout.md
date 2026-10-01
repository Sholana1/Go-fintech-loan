# External payout with an unknown outcome

## Symptoms

- Alert `LoanPayoutUnresolved` (`loan_payout_oldest_unresolved_seconds` above 30 min) or an open exception `PAYOUT_UNRESOLVED`.
- Customer reports the transfer to their bank shows "processing".

## What is true while a payout is `SENDING` or `UNKNOWN`

- The amount is **held** in the customer's deposit account: not spent, not returned.
- The provider may or may not have paid.
- The platform sent the transfer **once** and now only asks about it.

## Diagnose

```sql
SELECT payout_id, reference, provider_ref, state, last_code, attempts, state_changed_at, next_attempt_at
  FROM lending.payouts WHERE loan_id = '<loan id>';
```

Ask the provider about `reference` through their operations channel. Get a written answer with their own reference.

## Act

| Provider's answer | Action |
|---|---|
| Succeeded | Usually nothing: the next status query or the settlement report captures the hold. If queries keep failing, fix connectivity; do not capture by hand. |
| Failed, definitively | The status query or a callback releases the hold. Confirm the customer's available balance afterwards. |
| No record, same day | Wait. "Not found" is not proof it will not appear. |
| No record after the settlement report for that day | If the provider's contract makes the report final and `AbsentFromReportMeansFailed` is enabled, reconciliation releases the hold. Otherwise obtain written confirmation from the provider, then have engineering apply the release through the normal path. |

Resolve the `PAYOUT_UNRESOLVED` exception with the provider's reference in the note.

## Late success after a recorded failure

Exception `PAYOUT_LATE_SUCCESS`. The provider paid after we released the hold.

| Case | System behaviour | Operator |
|---|---|---|
| Customer still has the funds | Late-capture journal debits them; payout becomes `SUCCEEDED`; exception closes by itself | Nothing |
| Customer no longer has the funds | Debit refused; payout stays `FAILED`; exception stays open with the amount | This is a receivable from the customer. Finance and collections decide: recover from the receiving bank, agree repayment, or write off. The correcting entry is theirs to approve. |

## Never

- Release a hold because "it has been a long time".
- Send the transfer again, by any route.
- Tell the customer it failed before the provider has said so.
