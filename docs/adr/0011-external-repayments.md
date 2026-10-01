# 0011: External repayments are verified with the provider and credited to the deposit account first

**Context.** A customer can repay from outside the bank through a payment provider. Three parties can claim a payment happened: the customer's app, a webhook, and the provider's API.

**Decision.**
1. We issue the reference (`rp-<uuid>`) and store the payment as `INITIATED` before the customer pays.
2. Only the provider's answer to our own authenticated status call moves money. A webhook, even correctly signed, only triggers that call sooner; a poller triggers it anyway.
3. A confirmed payment is first credited to the customer's deposit account (`Dr SYS:COLLECTIONS_CLEARING / Cr customer deposit`, idempotent on the payment id), then applied by the ordinary repayment path (repayment id = payment id).
4. The amount credited is the amount the provider collected, not the amount announced.
5. `FAILED` and `EXPIRED` are not final against evidence: a later provider `SUCCESS` is still credited.

**Why credit the deposit account instead of the loan directly.** Money that arrived is the customer's whatever the loan needs. Overpayments, payments for a loan settled in the meantime, and payments that cannot be allocated all end in the right place with no refund process, and allocation rules exist in one code path.

**Alternatives rejected.**
- *Trust the signed webhook's amount and status.* One leaked webhook secret, or one provider bug, would mint money. A status call costs one request.
- *Credit the loan receivable directly.* Needs a second allocation path and a refund flow for surpluses.
- *Expire unpaid references permanently.* A customer who pays a minute late would lose the money until manual repair.

**Consequences.**
- Lending holds a ledger right (`EXTERNAL_COLLECTION`) that credits customers from a system account. It is bounded by journal shape, by one posting per payment id, and by reconciling `SYS:COLLECTIONS_CLEARING` against confirmed payments.
- Provider settlement into the bank account (`Dr cash at bank / Cr collections clearing`) and provider fees are not modelled yet, so the clearing balance equals the sum of confirmed payments. Matching it to the provider's settlement files is a launch dependency.
- A payment the provider confirms in another currency goes to `REVIEW` with an exception; nothing is credited by rule.
