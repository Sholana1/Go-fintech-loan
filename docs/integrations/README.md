# External integrations

Every external system is reached through a small interface (a *port*) declared by the service that needs it, and an *adapter* that makes real HTTP calls. Two kinds of adapter exist and they are configured separately so one cannot be mistaken for the other.

| Kind | Talks to | Configured by | Allowed in |
|---|---|---|---|
| Simulator adapter (`*sim`) | [`cmd/providersim`](../../cmd/providersim/main.go), this project's own simulator | `PROVIDER_SIM_URL`, `PROVIDER_SIM_API_KEY`, `PROVIDER_SIM_CALLBACK_SECRET` | `APP_ENV=local` or `test` only; startup fails elsewhere |
| Production adapter | A real provider | That provider's own settings (`PAYSTACK_*`) | Any environment |

## Status of each integration

| Capability | Port | Simulator adapter | Production adapter | Honest status |
|---|---|---|---|---|
| KYC / identity (BVN) | [`app.IdentityVerifier`](../../services/identity/app/ports.go) | [`bvnsim`](../../services/identity/providers/bvnsim/bvnsim.go) | none | **Launch dependency.** BVN validation is a NIBSS service reached through a licensed institution or an identity vendor; none publishes a contract that could be verified, so no adapter was written against guessed endpoints. |
| Credit bureau | [`app.CreditBureau`](../../services/lending/app/ports.go) | [`bureausim`](../../services/lending/providers/bureausim/bureausim.go) | none | **Launch dependency.** Needs a bureau's data-sharing agreement, documentation and sandbox. |
| Bank-account verification (name enquiry) | `app.PayoutProvider.ResolveAccount` | [`payoutsim`](../../services/lending/providers/payoutsim/resolve.go) | [`paystack`](../../services/lending/providers/paystack/resolve.go) | Written from Paystack's public OpenAPI spec. **Never run against Paystack.** |
| Loan disbursement to another bank | `app.PayoutProvider.Send` / `Query` / `SettlementReport` | [`payoutsim`](../../services/lending/providers/payoutsim/transfers.go) | [`paystack`](../../services/lending/providers/paystack/transfer.go) | Same. |
| Repayment / payment verification | [`app.PaymentVerifier`](../../services/lending/app/ports.go) | [`collectsim`](../../services/lending/providers/collectsim/collectsim.go) | [`paystack`](../../services/lending/providers/paystack/collection.go) | Same. |
| Webhooks | `httpapi.PayoutCallbackVerifier`, `httpapi.PaymentWebhookVerifier` | [`simsig`](../../services/lending/providers/simsig/simsig.go) | [`paystack.Webhooks`](../../services/lending/providers/paystack/webhook.go) | Paystack's signing scheme is **not in the OpenAPI spec**; implemented from published documentation as recalled, off by default (`PAYSTACK_WEBHOOKS_ENABLED`). |

"Written from the spec, never run" means: contract tests run the adapter against a local HTTP server that answers with the shapes the specification describes ([`paystack/*_test.go`](../../services/lending/providers/paystack/)). That proves the adapter sends what the spec asks for and reads what it promises. It does not prove it works against Paystack. A sandbox certification run with real test credentials is required first, and no live or sandbox call has been made from this project.

Source for the Paystack contract: `github.com/PaystackOSS/openapi` (`dist/paystack.yaml`), read 2026-10-01.

## Where the HTTP call is made

| Adapter | The line that leaves the process |
|---|---|
| Paystack (all operations) | `c.http.Do(req)` in [`paystack/client.go`](../../services/lending/providers/paystack/client.go), function `call` |
| BVN simulator | `c.http.Do(req)` in [`bvnsim.go`](../../services/identity/providers/bvnsim/bvnsim.go), `VerifyBVN` |
| Bureau simulator | `c.http.Do(req)` in [`bureausim.go`](../../services/lending/providers/bureausim/bureausim.go), `FetchReport` |
| Payout simulator | `c.http.Do(req)` in [`payoutsim.go`](../../services/lending/providers/payoutsim/payoutsim.go), `do` |
| Collection simulator | `c.http.Do(req)` in [`collectsim.go`](../../services/lending/providers/collectsim/collectsim.go), `VerifyPayment` |

No use case calls `net/http` directly, and no database transaction is open while any of these calls is in flight.

## Paystack: request by request

All requests: `Authorization: Bearer <PAYSTACK_SECRET_KEY>`, `Accept: application/json`, HTTPS only (the adapter refuses a plain-HTTP base URL unless it is loopback). Every response is the envelope `{"status": bool, "message": string, "data": …}`. Amounts are integers in kobo.

| Operation | Request | What the adapter reads | Timeout | Retried? | Idempotency |
|---|---|---|---|---|---|
| Name enquiry | `GET /bank/resolve?account_number=&bank_code=` | `data.account_name`, `data.account_number` | 20 s (`PayoutTimeout`) | No. The customer retries the acceptance. | A read. |
| Register recipient | `POST /transferrecipient` `{type:"nuban", name, account_number, bank_code, currency}` | `data.recipient_code` | shares the send deadline | No | Moves no money; safe to repeat. |
| Transfer | `POST /transfer` `{source:"balance", amount, recipient, reason, reference, currency}` | `data.status`, `data.reference`, `data.transfer_code`, `data.amount`, `data.currency` | 20 s | **Never.** One send per payout, ever. | `reference` is ours (`lp-<uuid>`), fixed in the database before the call. |
| Transfer status | `GET /transfer/verify/{reference}` | `data.status`, `data.reference`, `data.transfer_code` | 20 s | Yes, by the payout driver: 5 s, 15 s, 1 m, 5 m, 15 m, then every 15 m | A read. |
| Transfer list | `GET /transfer?from=&to=&per_page=100&page=N` | `data[]`, `meta.pageCount` | caller's | The reconciliation job runs again | A read. All pages or an error; never a partial report. |
| Payment verification | `GET /transaction/verify/{reference}` | `data.status`, `data.reference`, `data.amount`, `data.currency`, `data.id` | 15 s (`CollectionTimeout`) | Yes, by the payment driver: 15 s, 30 s, 1 m, 2 m, 5 m | A read. `reference` is ours (`rp-<uuid>`). |

### How answers are interpreted

| Answer | Transfer (`Send`) | Transfer status (`Query`) | Payment verification |
|---|---|---|---|
| `status: success` | SUCCESS: capture the hold | SUCCESS | SUCCESS: credit `data.amount` |
| `status: failed` or `reversed` | FAILED: release the hold | FAILED | FAILED |
| any other documented status | PENDING: funds stay held | PENDING | PENDING (`abandoned` = not paid yet) |
| a status not in the spec | error → unknown | error → unknown | error → unknown |
| HTTP 404 | error → unknown | NOT_FOUND (still unknown) | NOT_FOUND (not paid yet) |
| timeout, connection error, 5xx, other 4xx, bad JSON | error → unknown | error → unknown | error → unknown |
| body about a different reference, amount or currency | error → unknown | error → unknown | error → unknown |
| recipient registration fails for any reason | **FAILED** (certain: no transfer was attempted) | — | — |

"Unknown" is a state, not a failure: the customer's money stays held (payout) or uncredited (payment) and the status call is repeated. Unknown never becomes failed by itself.

The spec lists transfer statuses (`pending, success, failed, otp, abandoned, reversed, blocked, rejected, received`) without defining them. Only `success`, `failed` and `reversed` are treated as final. Treating `blocked` or `rejected` as failed would release a customer's funds on a guess; confirming their meaning is part of sandbox certification.

### Webhooks (unverified, off by default)

Header `x-paystack-signature` = hex HMAC-SHA512 of the raw body with the secret key. Only `data.reference` is read. A webhook never states an outcome: it makes the service call `GET /transfer/verify/{reference}` or `GET /transaction/verify/{reference}` and apply that answer. If webhooks are disabled, wrong or lost, the polling schedule reaches the same result later.

## Simulator contract

See [../contracts/provider-simulator.md](../contracts/provider-simulator.md).

## Running against Paystack's test mode (not done here)

```bash
LENDING_PAYOUT_PROVIDER=paystack
LENDING_COLLECTION_PROVIDER=paystack
PAYSTACK_SECRET_KEY=sk_test_...      # from a secret manager, never committed
PAYSTACK_WEBHOOKS_ENABLED=false
```

The bureau and BVN providers remain simulators, so this still requires `APP_ENV=local`. Nothing in this repository has made that call; doing so sends requests to a third party under your account and is your decision to make.

## What a production adapter needs before it can be written

| Integration | Needed |
|---|---|
| BVN / NIN verification | Provider agreement, API documentation, sandbox credentials, liveness requirements |
| Credit bureau | Data-sharing agreement, documentation, sandbox, whether an enquiry repeated with the same reference is one enquiry, monthly submission format |
| Paystack (before real use) | Sandbox run of every row in the tables above; meaning of the undefined statuses; which `POST /transfer` errors guarantee "not created"; webhook signature confirmation; OTP disabled for server-initiated transfers; settlement and balance funding process |
