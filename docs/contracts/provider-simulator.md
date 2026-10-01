# Provider simulator contract

**This is not any real provider's API.** It is the contract of this project's simulator ([simulator/](../../simulator/server.go)), run locally by [`cmd/providersim`](../../cmd/providersim/main.go). For the real provider that has an adapter, see [../integrations/README.md](../integrations/README.md).

All requests carry `X-Api-Key`.

## Identity (BVN) verification

`POST /kyc/v1/bvn-verifications` — headers `X-Api-Key`, `Idempotency-Key: <request_ref>`; body `request_ref`, `bvn`, `full_name`, `date_of_birth`.
Response: `verification_ref`, `result` (`MATCH` \| `MISMATCH` \| `NOT_FOUND`). The same `request_ref` returns the same verification.

| BVN ends | Behaviour |
|---|---|
| `0000` | `NOT_FOUND` |
| `1111` | `MISMATCH` |
| `9999` | never answers (timeout) |
| `8888` | malformed JSON |
| `7777` | HTTP 500 |
| other | `MATCH` |

## Credit bureau

`POST /bureau/v1/reports` — body `request_ref`, `bvn`, `full_name`, `date_of_birth`.
Response: `report_ref`, `score` (integer 300–850, or `null` for a thin file), `active_loans`, `total_outstanding_minor`, `monthly_obligations_minor`, `worst_delinquency_days_12m`, `as_of`.
The same `request_ref` returns the same report.

| BVN ends | Behaviour |
|---|---|
| `4201` / `4202` / `4203` | score 640 / 560 / 480 |
| `4204` | thin file (no score) |
| `4205` | 120 days delinquent |
| `4206` | ₦90,000 monthly obligations |
| `4207` | never answers (timeout) |
| `4208` | malformed JSON |
| `4209` | HTTP 500 |
| `4210` | HTTP 503 twice, then answers |
| other | score 720 |

## Payout provider

| Call | Behaviour |
|---|---|
| `POST /payout/v1/accounts/resolve` | `account_name`, `enquiry_ref`; `404` if not found |
| `POST /payout/v1/transfers` | Idempotent on `reference`. Returns `status` `SUCCESS` \| `FAILED` \| `PENDING`, `provider_ref`, `code` |
| `GET /payout/v1/transfers/{reference}` | Current status; `404` if unknown |
| `GET /payout/v1/settlement-report?date=YYYY-MM-DD` | Final-status transfers created on that Lagos date |

| Account number ends | Behaviour |
|---|---|
| `90` | rejected (`FAILED`) |
| `91` | processed successfully, response never arrives |
| `92` | failed, response never arrives |
| `93` | `PENDING`, becomes `SUCCESS` after two status queries |
| `94` | processed successfully, malformed response body |
| `95` | HTTP 500, nothing recorded |
| `96` | `PENDING` until resolved by the test |
| `97` | name enquiry never answers |
| `98` | name enquiry: not found |
| `99` | name enquiry: a different person's name |
| other | success |

## Callbacks

`POST <callback url>` with body `event_id`, `reference`, `status`, `provider_ref`, `code`, `occurred_at` and headers:

- `X-Sim-Timestamp`: Unix seconds
- `X-Sim-Signature`: hex HMAC-SHA256 of `"<timestamp>.<raw body>"` with the shared secret

The receiver verifies the signature over the raw bytes in constant time, rejects timestamps more than five minutes from its clock, and only then parses the body. Duplicates (same `event_id`) are acknowledged with `200` and have no effect.

## Inbound payments (external repayments)

| Call | Behaviour |
|---|---|
| `GET /collections/v1/payments/{reference}` | The provider's record of a payment: `reference`, `status` (`SUCCESS` \| `FAILED` \| `PENDING`), `amount_minor`, `currency`, `provider_ref`, `code`, `paid_at`; `404` if the reference has not been paid |
| `POST /collections/v1/sandbox/payments` | **Manual testing only.** Stands in for the provider's checkout page: body `reference`, `amount_minor`, optional `currency` (default `NGN`) and `status` (default `SUCCESS`). Records the payment and sends the webhook. |

Webhook: `POST <collection webhook url>` with body `event_id`, `type` (`payment.updated`), `reference`, `occurred_at`, signed exactly like a payout callback. It carries no amount and no status on purpose: the receiver must ask `GET /collections/v1/payments/{reference}`.

