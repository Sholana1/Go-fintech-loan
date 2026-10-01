# Ledger discrepancy

## Symptoms

- `ledger_invariant_violations` above zero for any `kind`. **Page.**
- Exception `CONTROL_ACCOUNT_MISMATCH` or `JOURNAL_MISMATCH`.

## Severity

| Signal | What it means | Urgency |
|---|---|---|
| `unbalanced_journal`, `balance_drift`, `running_balance_break`, `held_drift`, `trial_balance_difference` | The ledger disagrees with itself. The database constraints should make this impossible; if it happened, something bypassed them. | Incident. Stop postings to the affected accounts. |
| `default_partition_rows` | A month partition was missing; rows landed in the default partition. Correctness is intact. | Same day: run `ledgerd migrate` and move the rows. |
| `CONTROL_ACCOUNT_MISMATCH` | Lending's loan book total differs from the ledger control account at a quiet moment | Same day. Money is right in the ledger; find which side is wrong. |
| `JOURNAL_MISMATCH` | A posting lending recorded is missing or different in the ledger | Incident. |

## Diagnose

```bash
ledgerd verify        # recomputes every invariant from the immutable entries
```

```sql
-- first entry at which an account's running balance diverges
SELECT entry_id, journal_id, posted_at, direction, amount, balance_after,
       sum(CASE WHEN direction = a.normal_side THEN amount ELSE -amount END)
         OVER (ORDER BY entry_id) AS recomputed
  FROM ledger.entries e JOIN ledger.accounts a USING (account_id)
 WHERE a.code = '<account code>' ORDER BY entry_id;

-- control account vs loan book
SELECT * FROM lending.recon_exceptions WHERE state = 'OPEN' ORDER BY opened_at;
```

For a control mismatch, the exception's `amount_minor` is ledger minus loan book. Look for a manual change to `lending.instalments` (the audit log will not have it) or a loan whose state was changed outside the application.

## Repair

Entries are the truth; balances are a projection maintained by a trigger. If a projection were ever wrong:

1. Stop postings to the affected accounts (set status `FROZEN`).
2. Recompute from entries and compare (`ledgerd verify`).
3. Find the cause before changing anything.
4. A repair that sets the projection from the recomputation is an owner-role operation requiring two people and a written incident record. **No tool for this ships in the repository**: it should be written for the specific incident and reviewed.

A wrong *entry* is never edited. It is reversed by a new journal that references it, with maker-checker and finance approval.

## After

Resolve each exception with a note. Reconciliation closes `CONTROL_ACCOUNT_MISMATCH` by itself once the two sides agree.
