# Lending REST API (v1)

Base path `/v1`. JSON. Money is integer minor units (kobo) with an ISO currency code.

## Conventions

| Topic | Rule |
|---|---|
| Authentication | `Authorization: Bearer <token>` issued by identity (`POST /v1/sessions`). Tokens last 15 minutes. |
| Resource authorisation | A customer can address only their own resources. Another customer's id returns `404 NOT_FOUND`, the same as an id that does not exist. Staff endpoints require a role. |
| Idempotency | `Idempotency-Key` header (8–128 characters) is required on every POST that creates something or moves money. Same key and body: the original resource is returned with `Idempotent-Replay: true`. Same key, different body: `422 IDEMPOTENCY_KEY_REUSED`. Keys are scoped to the caller and kept 24 hours. |
| Errors | `{"error":{"code":"…","message":"…","request_id":"…"}}`. Codes are stable; messages are not. |
| Unknown fields | Rejected with `400 INVALID_JSON`. |
| Versioning | Fields are only added within `/v1`. |

## Customer endpoints

| Method and path | Purpose | Success |
|---|---|---|
| `POST /loan-applications` | Apply (T0). Body: `product_id`, `amount_minor`, `currency`, `tenor_months`, `stated_monthly_income_minor`, `consent_credit_check` | `202` application |
| `GET /loan-applications/{id}` | Status, reason codes, offer when ready | `200` |
| `POST /loan-applications/{id}/cancel` | Withdraw before acceptance | `200` |
| `GET /loan-offers/{id}` | Offer with its disclosure document and hash | `200` |
| `POST /loan-offers/{id}/accept` | Accept (T2). Body: `disclosure_hash`, `pin`, `auto_debit_authorised`, `destination` | `201` loan (disbursed) or `202` (disbursing) |
| `GET /loans` | The customer's loans | `200` |
| `GET /loans/{id}` | Loan with schedule and payout status | `200` |
| `GET /loans/{id}/payoff-quote` | Cost to settle today | `200` |
| `GET /loans/{id}/statement` | Schedule, repayments, fees, totals | `200` |
| `POST /loans/{id}/repayments` | Repay from the deposit account. Body: `amount_minor`, `currency` | `201` applied, `202` processing |
| `POST /loans/{id}/external-repayments` | Start a repayment from outside the bank. Body: `amount_minor`, `currency`. Moves no money; returns the `reference` to pay with at the provider | `201` payment |
| `GET /loans/{id}/external-repayments/{payment_id}` | Status of that payment | `200` |

`destination` is `{"type":"DEPOSIT_ACCOUNT"}` or `{"type":"EXTERNAL_BANK_ACCOUNT","bank_code":"…","account_number":"…"}`.

### Statuses shown to customers

| Resource | Statuses |
|---|---|
| Application | `PROCESSING`, `UNDER_REVIEW`, `OFFER_READY`, `DISBURSING`, `DISBURSED`, `DECLINED`, `EXPIRED`, `CANCELLED`, `FAILED` |
| Loan | `DISBURSING`, `ACTIVE`, `OVERDUE`, `CLOSED`, `IN_RECOVERY`, `DISBURSEMENT_FAILED` |
| Repayment | `PROCESSING`, `SUCCESSFUL`, `FAILED` |
| External repayment | `AWAITING_PAYMENT`, `PROCESSING`, `APPLIED`, `CREDITED_TO_ACCOUNT`, `FAILED`, `EXPIRED` |
| Payout | `PROCESSING`, `SENT`, `FAILED`, `CANCELLED` |

A loan is `ACTIVE` only once the ledger has posted the disbursement. A payout whose outcome is unknown is `PROCESSING`, never `FAILED`.

An external repayment is credited only after the provider itself confirms it. `APPLIED` means it was allocated to the loan; `CREDITED_TO_ACCOUNT` means the money arrived and stayed in the customer's deposit account because the loan no longer needed it. `received_minor` is what the provider collected, which may differ from `expected_minor`. A `FAILED` or `EXPIRED` payment still becomes `APPLIED` if the provider later proves the money arrived.

## Staff endpoints

| Method and path | Role | Purpose |
|---|---|---|
| `GET /ops/manual-reviews` | `credit_reviewer` | Referred applications with their decision snapshots |
| `POST /ops/manual-reviews/{application_id}/decision` | `credit_reviewer` | Approve (amount, risk band, note) or decline (note) |
| `POST /ops/admin-actions` | `ops_maker` | Propose `WRITE_OFF` or `RESTRUCTURE` |
| `POST /ops/admin-actions/{id}/approve` · `/reject` | `ops_checker` | Decide; the checker must differ from the maker |
| `GET /ops/admin-actions` | any ops role | Actions awaiting approval |
| `GET /ops/recon-exceptions?state=OPEN` | any ops role | Reconciliation exceptions |
| `POST /ops/recon-exceptions/{id}/resolve` | `ops_checker` | Close with a note |
| `GET /ops/portfolio` | any ops role | Loan book by state and arrears bucket |
| `GET /ops/loans/{id}` | any ops role, `credit_reviewer` | Any loan with its schedule |

## Provider notifications

Authenticated by the provider's signature, not by bearer token. `401 CALLBACK_REJECTED` if authentication fails; `200` if handled, including duplicates; `5xx` if it could not be recorded (the provider retries).

| Path | From | Effect |
|---|---|---|
| `POST /provider-webhooks/payouts` | Payout provider | Applies, or looks up, the outcome of a transfer |
| `POST /provider-webhooks/payments` | Payment provider | Triggers verification of an inbound payment. The body's claims about amount or status are ignored |

Signature schemes: [provider-simulator.md](provider-simulator.md) for the simulator, [../integrations/README.md](../integrations/README.md) for Paystack.

## Rate limits

| Endpoint | Counted per | Default |
|---|---|---|
| `POST /loan-applications` | customer | 5 per minute (`LENDING_APPLICATION_RATE_MAX`, `_WINDOW`) |
| identity `POST /sessions`, `/staff/sessions` | phone number or email | 10 per minute (`IDENTITY_LOGIN_RATE_MAX`, `_WINDOW`) |

Over the limit: `429 RATE_LIMITED` with `Retry-After`. Counters live in Redis. If Redis is unreachable the request is allowed (see [ADR 0012](../adr/0012-redis-rate-limiting.md)).

## Error codes

| HTTP | Code | Meaning | Retry? |
|---|---|---|---|
| 400 | `VALIDATION_FAILED`, `INVALID_JSON` | The request is wrong | No |
| 401 | `UNAUTHENTICATED` | Missing, invalid or expired token | After signing in |
| 403 | `FORBIDDEN` | Wrong kind of principal, missing role, or a rule forbids it | No |
| 403 | `STEP_UP_FAILED` | Wrong PIN | With the right PIN |
| 404 | `NOT_FOUND` | Does not exist, or is not yours | No |
| 409 | `APPLICATION_ALREADY_OPEN` | One application per product at a time | No |
| 409 | `OFFER_NOT_OPEN` | Already accepted, voided or cancelled | No |
| 409 | `OPERATION_IN_PROGRESS` | Another operation on the loan is being posted | Yes, shortly, same key |
| 409 | `LOAN_NOT_REPAYABLE` | Nothing is payable, or the loan is closed | No |
| 409 | `CONFLICT` | The resource is not in a state that allows this | No |
| 410 | `OFFER_EXPIRED` | The offer has expired | No; apply again |
| 422 | `IDEMPOTENCY_KEY_REUSED` | Key used before with a different body | No |
| 422 | `DISCLOSURE_MISMATCH` | Hash does not match the offer | No |
| 422 | `DESTINATION_NOT_VERIFIED` | External account is not the customer's own | No |
| 422 | `INSUFFICIENT_FUNDS` | The debit was refused | When funded, new key |
| 429 | `CREDENTIAL_LOCKED` | Too many wrong PINs | Later |
| 429 | `RATE_LIMITED` | Too many requests of this kind | After `Retry-After` seconds |
| 401 | `CALLBACK_REJECTED` | Provider notification failed authentication | No |
| 503 | `DEPENDENCY_UNAVAILABLE` | A required service did not answer; nothing was applied | Yes, same key |
