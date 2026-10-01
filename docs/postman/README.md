# Postman: personal loans (local)

| File | What it is |
|---|---|
| [personal-loans.postman_collection.json](personal-loans.postman_collection.json) | 48 requests in 8 folders, in journey order, each with assertions and a saved example response recorded from a real local run |
| [local.postman_environment.json](local.postman_environment.json) | URLs of the local services and the simulator's API key and webhook secret (local development values only) |

Everything runs against local processes and the provider **simulator**. No real money, customer or provider is involved.

## Before you start

```bash
make infra-up dev-setup migrate topics
make run-ledger      # four terminals
make run-sim
make run-identity
make run-lending
```

Import both files into Postman and select the environment **Bank platform local**.

## Order

Run the folders top to bottom. Requests store what later requests need (token, application id, offer id, disclosure hash, loan id, payment reference) in collection variables, so nothing is copied by hand.

| Folder | What it shows |
|---|---|
| 1. Onboarding | Registration with BVN verification over HTTP; sign-in; validation, duplicate and not-verified errors |
| 2. Application and decision | `202` and asynchronous assessment; `401`; `400`; idempotent retry; same key with a different body; one open application |
| 3. Offer, acceptance and disbursement | Step-up with PIN; disclosure hash; the loan is `ACTIVE` only when the ledger has posted; a retried acceptance does not disburse twice |
| 4. Repayment from the deposit account | Partial repayment, idempotent retry |
| 5. Repayment from outside the bank | A forged webhook is refused; a genuine webhook with no payment behind it credits nothing; after the simulated checkout the payment is verified, credited and applied |
| 6. Full repayment and closure | Payoff quote, settlement, closed loan refuses further repayments |
| 7. Disbursement to another bank | Name enquiry must match the customer; hold, send, capture |
| 8. Staff | Needs a staff account (below) |

Requests named "repeat until …" poll: in the Collection Runner they repeat automatically; when sending by hand, press Send again until the status changes.

A token lasts 15 minutes. If a request returns `401 UNAUTHENTICATED`, send **Sign in** again.

## Staff account (folder 8)

```bash
set -a; . .dev/local.env; set +a
STAFF_EMAIL=ops@bank.test STAFF_PASSWORD=correct-horse-battery STAFF_ROLES=ops_viewer,ops_checker go run ./cmd/identityd create-staff
```

## Making things fail on purpose

The simulator chooses its behaviour from the last digits of the data you send ([provider-simulator.md](../contracts/provider-simulator.md)).

| To see | Change |
|---|---|
| Identity provider timeout → `503`, no customer created | BVN ending `9999` in **Register customer** |
| Declined application | Register with a BVN ending `4203` (score 480), then apply |
| Higher price for a weaker score | BVN ending `4201` (score 640: 6% a month) or `4202` (score 560: 8% a month) |
| Application referred to manual review (`UNDER_REVIEW`) | BVN ending `4204` (no bureau score), or `amount_minor` 40000000 with a high stated income (above the automatic approval limit) |
| Amount reduced for affordability | BVN ending `4206` and `amount_minor` 30000000 |
| Credit bureau outage: application stays `PROCESSING`, then recovers or expires | BVN ending `4207` (timeout) or `4210` (two failures, then success) |
| Payout rejected: funds released, loan stays active | External account number ending `90` |
| Payout timeout with a successful transfer: stays `PROCESSING`, then `SENT` after the status query | Account number ending `91` |
| Payout pending forever (`PROCESSING`) | Account number ending `96` |
| Failed external payment | `"status": "FAILED"` in **SIMULATOR: customer pays at the provider** |
| Customer paid a different amount | Change `amount_minor` in the same request; `received_minor` shows what was credited |
| Rate limit | Send **Apply for a loan** six times within a minute: the sixth is `429 RATE_LIMITED` with `Retry-After` |
| PIN lockout | Send **Sign in: wrong PIN** five times: `429 CREDENTIAL_LOCKED` for 15 minutes |

## Running it from the command line

```bash
npx newman run docs/postman/personal-loans.postman_collection.json -e docs/postman/local.postman_environment.json --delay-request 100
```

Last run of this command on 2026-10-01: 48 requests (more when a polling request repeats), 81 assertions, 0 failed.

## Checking the result in the database

```bash
docker exec -it bankplatform-postgres-1 psql -U postgres -d lending
```

```sql
SELECT state, state_reason FROM lending.applications ORDER BY submitted_at DESC LIMIT 1;
SELECT kind, journal_type, state, journal_id FROM lending.posting_intents ORDER BY created_at DESC LIMIT 5;
SELECT source, applied_minor, unapplied_minor, state FROM lending.repayments ORDER BY created_at DESC LIMIT 5;
SELECT reference, state, expected_minor, verified_minor, last_code FROM lending.external_payments ORDER BY created_at DESC LIMIT 5;
SELECT event_type, published_at IS NOT NULL AS published FROM lending.outbox ORDER BY outbox_id DESC LIMIT 10;
```

Ledger (`-d ledger`):

```sql
SELECT j.journal_id, j.journal_type, a.code, e.direction, e.amount
  FROM ledger.journals j JOIN ledger.entries e USING (journal_id) JOIN ledger.accounts a USING (account_id)
 ORDER BY j.journal_id DESC, e.direction DESC LIMIT 12;
```

And the invariants: `go run ./cmd/ledgerd verify` must print all zeros.
