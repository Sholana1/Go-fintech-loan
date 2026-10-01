# Runbooks: personal loans

| Situation | Runbook |
|---|---|
| A loan was accepted but not disbursed | [failed-or-stuck-disbursement.md](failed-or-stuck-disbursement.md) |
| An external payout has an unknown outcome | [uncertain-payout.md](uncertain-payout.md) |
| Provider callbacks are duplicated, rejected or contradictory | [payout-callbacks.md](payout-callbacks.md) |
| A customer paid from outside the bank and was not credited | [external-payment.md](external-payment.md) |
| A repayment and the loan book disagree | [repayment-discrepancy.md](repayment-discrepancy.md) |
| Ledger and loan book disagree; ledger invariant alert | [ledger-discrepancy.md](ledger-discrepancy.md) |
| Database failover or restore | [database-recovery.md](database-recovery.md) |

## Rules for every manual action

1. **Never edit ledger rows or balances.** They are immutable and the database refuses. A correction is a new journal.
2. **Never mark a payout failed or release a hold on a guess.** A timeout is an unknown outcome. Use the provider's status query or settlement report.
3. **Never re-send a payment with a new reference.**
4. **Two people.** Write-offs and restructures already require maker and checker in the system. Any correcting journal outside those flows needs the same, plus finance sign-off, and is not something this codebase lets one person do.
5. **Leave evidence.** Resolve the reconciliation exception with a note naming what you checked (provider reference, ticket, statement line). The audit log records who did it.
6. **No personal data in tickets or chat.** Refer to customers, loans and payouts by id.
