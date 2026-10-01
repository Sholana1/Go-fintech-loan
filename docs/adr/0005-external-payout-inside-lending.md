# 0005: Loan payout and reconciliation live in lending for now

**Plan.** `payments` owns rail integrations; `recon` owns reconciliation.

**Decision.** The personal-loan product needs exactly one outbound movement (loan proceeds to the customer's own external account) and reconciliation of exactly the accounts lending posts to. Both are implemented in `services/lending/app` behind the `PayoutProvider` and `Ledger` ports.

**Why not build `payments` now.** The instruction for this phase is to build only what personal loans need. A general transfers service is the next product.

**What carries over unchanged** when `payments` is built: the state machine (`READY → HELD → SENDING → SUCCEEDED | FAILED | UNKNOWN`), the rule that a timeout is an unknown outcome, the single `applyProviderOutcome` decision point, and the tests.

**Disbursement is two steps, as in the plan.** T3 is the credit to the customer's deposit account. The external leg is a separate payout (T4). A failed payout leaves the money in the deposit account; it never un-disburses the loan.
