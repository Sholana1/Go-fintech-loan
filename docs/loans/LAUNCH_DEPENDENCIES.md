# Personal loans: what stands between this code and a launch

Passing the implementation gate means the product works, correctly, against simulators on a developer's machine. It does **not** mean it may handle real customers or money. Each item below is required first, with an owner outside engineering where noted.

## Blocking: partner integrations (none is verified against a real provider)

| Integration | Current state | Needed |
|---|---|---|
| BVN / NIN verification and liveness | HTTP simulator; no production adapter | Provider contract, documentation, sandbox, certification. The plan's compliance matrix requires liveness at online account opening (CBN circular of 12 March 2026, as reported; unverified against the primary text). |
| Credit bureau | HTTP simulator; no production adapter | Data-sharing agreement, documentation, sandbox. Confirm whether an enquiry repeated with the same reference is idempotent. Build the monthly data submission (not implemented). |
| Payout rail | HTTP simulator, plus a Paystack adapter written from the public OpenAPI specification and never run against Paystack | Provider agreement; sandbox certification of every call; meaning of the transfer statuses the specification leaves undefined; which send errors guarantee "not created"; settlement-report finality; webhook signature confirmation. See [../integrations/README.md](../integrations/README.md). |
| Inbound payments (external repayments) | HTTP simulator, plus the same Paystack adapter (payment verification) | The same certification; provider settlement files and fees posted to the ledger and reconciled against `SYS:COLLECTIONS_CLEARING` (not built). |

## Blocking: legal and compliance confirmation

| Item | Why it matters here |
|---|---|
| Licensing position (own MFB licence vs partner) | Decides who books the loan and whose ledger is authoritative |
| FCCPC digital-lending rules: waiver and conduct requirements | Disclosure content, collections conduct |
| NDPA automated-decision rights | **A customer cannot yet request human review of an automated decline through the API.** The plan requires that path. |
| Credit Reporting Act: report before lending, report loans to bureaus | The first is enforced in code; the second is not built |
| KYC tier limits on balances and transactions | **Not enforced by the ledger yet** (planned with transfers). A loan credit could exceed a tier's balance limit. |
| Reported 24-hour ₦20,000 cap on new accounts and new devices | Not enforced; whether it applies to loan credits is an open question in the plan |
| Product terms | Rates, fees, limits, the non-refundable origination fee, no partial prepayment, late-fee design: see "Decisions for product and legal" in [PRODUCT_RULES.md](PRODUCT_RULES.md) |
| Consent wording and retention periods | The code records consent version and reference; the wording and retention schedule are not defined |
| Hosting location and cross-border transfer basis | The plan's largest open risk |

## Blocking: security

| Item | State |
|---|---|
| Production certificate authority and rotation for workload mTLS | Not chosen |
| Key management for token signing and BVN encryption | Keys are read from files and environment variables; must come from a managed key service |
| Staff authentication | Development-only password login; production needs SSO with MFA |
| Customer authentication | PIN and 15-minute tokens only: no device binding, no MFA, no refresh or revocation |
| Edge rate limiting, WAF | Sign-in and loan-application limits exist per customer (Redis). Per-IP limits and a WAF need the gateway, which does not exist yet |
| Independent security review and penetration test | Not done |
| `GO-2026-6443` (gRPC) | Accepted with an expiry; upgrade when a fixed release is tagged |

## Blocking: operations

| Item | State |
|---|---|
| Infrastructure as code, managed PostgreSQL with synchronous standby, managed Kafka | None provisioned |
| Backup, point-in-time restore and failover exercises with evidence | None run |
| Load test against agreed targets on production-shaped infrastructure | Only a laptop benchmark with simulated providers exists |
| Alert routing, on-call rota, dashboards deployed | Rules and a dashboard definition exist; nothing is deployed |
| CI | Workflow written; it has not been executed on a CI runner |
| Container images | Dockerfile written; see the review package for whether it was built |
| Schema registry and compatibility gate for events | Not present |
| Query plans at realistic volume | Only inspected on a few hundred rows |

## Functional gaps inside the product

| Gap | Note |
|---|---|
| Human-review request for automated declines | Required by the plan |
| Monthly credit-bureau reporting | Required by law as reported |
| Expected-credit-loss provisioning | Finance's model; write-off is currently a direct expense |
| Direct-debit mandates on other banks, global standing instruction | Only own-account debit is implemented |
| Collections contact management (frequency, hours, channels) | Not built; no customer communication of any kind is sent |
| Customer notifications | Not built |
| Bureau report reuse across applications | Each application makes its own enquiry |
| Partial prepayment | Not offered |
| Regulatory returns | Not built |
