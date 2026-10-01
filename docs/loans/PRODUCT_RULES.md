# Personal loan: product rules (version 1)

These are the rules the code implements. Every number below comes from versioned configuration ([services/lending/config/](../../services/lending/config/)); the values shipped in the repository are **illustrative placeholders**, not approved pricing and not regulatory figures. Product, credit risk, legal and compliance must set and sign off real values before launch.

## Who can borrow

| Rule | Source |
|---|---|
| Verified KYC at or above the product's minimum tier | Identity service, read at decision time |
| Active customer with an open deposit account | Identity service |
| At least the product's minimum age | Date of birth from the verified record |
| No other loan in progress or outstanding with us | Loan book |
| No previous write-off with us | Loan book |
| Not blocked by fraud screening | Velocity rules |

A customer who fails any of these is declined **without** a credit-bureau enquiry.

## What the customer provides

Amount, tenor, stated monthly income, and consent to a credit check. Nothing else. The product does not collect contacts, messages, location or device content.

## The decision

1. Credit report from the bureau (required; if it cannot be obtained the application waits, then expires — it is never approved or declined on a guess).
2. Decline if the score is below the policy minimum or recent delinquency exceeds the policy maximum.
3. Price by score band.
4. Size the loan: the requested amount, capped by the product maximum, the exposure limit and affordability (existing obligations plus the new instalment must fit within the permitted share of income). The amount may be reduced; if even the product minimum does not fit, decline.
5. Refer to a person if the file is thin, fraud screening asks for review, or the amount exceeds the auto-approval ceiling.

A reviewer can settle a referral but cannot approve more than step 4 allows. Every decision, automated or human, is stored with exactly the inputs it used and can be replayed.

## The offer

| Term | Rule |
|---|---|
| Instalments | Equal monthly instalments: `A = P·r / (1 − (1+r)^−n)` |
| Interest per period | Opening principal × monthly rate |
| Principal per period | Instalment − interest; the final instalment repays the remainder exactly |
| Rounding | Nearest kobo, exact halves to the even kobo |
| Origination fee | Percentage of principal, deducted from the disbursement |
| Due dates | Instalment *k* falls due *k* months after acceptance (day of month kept; clamped to month end) |
| Cost disclosure | Monthly rate, nominal annual rate, and effective annual cost including the fee |
| Validity | The offer expires after the configured period |

The customer accepts by returning the hash of the disclosure they were shown, with their PIN.

## Disbursement

The loan is booked to the customer's own deposit account. It is reported as disbursed only once the ledger has posted the entry. Optionally the proceeds are then sent to the customer's own account at another bank; that account must pass a name enquiry matching the customer's verified name. Proceeds are never sent to a third party.

## Interest

Each period's interest is fixed when the schedule is built and is earned evenly across the days of that period. It is recognised daily. After an instalment's due date no further interest accrues on it: lateness is charged through the late fee, not through additional interest.

## Repayments

A repayment is taken from the customer's deposit account. The amount offered is an upper limit:

1. If it covers the **payoff figure** (all principal, interest earned to date, fees), the loan is settled. Interest not yet earned is waived. Anything above the payoff is not taken.
2. Otherwise amounts already due are paid first: fees, then interest, then principal, oldest instalment first.
3. What remains may pay ahead on the **next** instalment only: its interest earned so far, then its principal.
4. Anything still remaining is not taken.

Partial prepayment of later instalments is not offered in this version.

## Scheduled collection

If the customer authorised it at acceptance, on each due date the amount due is debited from their deposit account. It never takes more than is due, takes a partial amount if that is all there is (configurable), and retries a bounded number of times per day.

## Arrears

| Event | Rule |
|---|---|
| Days past due | Counted from the oldest unpaid due date; the due date itself is not past due |
| Late fee | Once per instalment unpaid beyond the grace period |
| Classification | Buckets by days past due, for operations |
| Restructure | Outstanding principal re-amortised over a new tenor; earned interest and fees carried forward; needs two people |
| Write-off | Permitted only beyond a minimum days past due; needs two people |
| Recovery | Money received after write-off is recorded as a recovery, up to the amount written off |

## Decisions for product and legal

These rules were chosen to make a coherent first product. Each changes what a customer pays and should be confirmed before launch.

1. **The origination fee is not refunded on early settlement**, including same-day settlement. A cooling-off right, if one applies, would change this.
2. **No partial prepayment.** The architecture plan described re-amortising on prepayment; version 1 offers full early settlement only.
3. **No interest on overdue amounts**; a flat late fee instead.
4. **Paying ahead does not reduce the current period's interest**, which is fixed at the period's start.
5. **Stated income is not verified** beyond the affordability check against bureau obligations.
6. **Customers cannot yet request human review of an automated decline** through the API. The plan requires that path.
