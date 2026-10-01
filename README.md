# Bank platform

A Nigerian digital-banking platform built product by product from an architecture plan. This repository currently implements the first product, **personal loans**, and the minimum financial foundation it needs.

Nothing here handles real money. External providers are reached over real HTTP through adapters. Locally they talk to a **simulator**; the services refuse to start with a simulator adapter outside local and test environments. A production adapter for Paystack (name enquiry, transfers, payment verification) is written from Paystack's public OpenAPI specification and has **never been run against Paystack**; identity verification and the credit bureau have no production adapter. See [docs/integrations/README.md](docs/integrations/README.md). See [docs/loans/REVIEW.md](docs/loans/REVIEW.md) for what is built, how it was verified, and what stands between this and a launch.

## What is in the repository

| Path | What it is |
|---|---|
| [services/ledger/](services/ledger/) | Double-entry ledger. The only writer of balances. gRPC. |
| [services/identity/](services/identity/) | Customers, KYC status, credentials, access tokens. REST for customers, gRPC internally. |
| [services/lending/](services/lending/) | Personal loans: application, assessment, offer, disbursement, servicing, collections, reconciliation. REST. |
| [simulator/](simulator/) | Deterministic HTTP simulator of the external providers (identity, bureau, payouts, inbound payments). |
| [platform/](platform/) | Small shared libraries: money, business calendar, outbox, Kafka, gRPC security, auth tokens, rate limiting, telemetry. |
| [proto/](proto/), [gen/](gen/) | Protocol Buffer contracts and generated Go. |
| [cmd/](cmd/) | Entry points: `ledgerd`, `identityd`, `lendingd`, `providersim`, `devsetup`, `loanbench`. |
| [deploy/](deploy/) | Local infrastructure (PostgreSQL, Kafka, Redis), alert rules, dashboard. |
| [docs/](docs/) | Decisions, contracts, integrations, product rules, runbooks, traceability, the review package, a Postman collection. |

Each service owns its database and its code under `services/<name>/`:

```
domain/      rules with no I/O (schedules, allocation, policy, state machines)
app/         use cases; transaction boundaries are visible here
postgres/    SQL, one statement per method, locking spelled out
httpapi/     REST transport        grpcapi/   gRPC transport
providers/   adapters for external systems (and the simulator)
migrations/  schema
<name>test/  a kit that starts the real service for other services' tests
```

## Run it locally

Requirements: Go 1.27+, Docker.

```bash
make tools          # pinned build tools into .tools/
make infra-up       # PostgreSQL on :55432, Kafka on :59092, Redis on :56379
make dev-setup      # certificates, keys and .dev/local.env
make migrate topics # schemas, dev funding account, Kafka topics
```

Then, in four terminals:

```bash
make run-ledger
make run-sim
make run-identity
make run-lending
```

Walk one customer through the product:

```bash
./scripts/local-scenario.sh
```

## Verify it

```bash
make test-unit   # no infrastructure needed
make test        # everything, with the race detector (needs `make infra-up`)
make lint        # gofmt, go vet, staticcheck, buf lint
make vuln        # govulncheck, with explicit expiring exceptions
make bench       # loan-journey timings against the running services
```

`make test` fails, rather than skips, if PostgreSQL, Kafka or Redis is unreachable.

To drive the API by hand, import [docs/postman/](docs/postman/) into Postman.

## Where to start reading

1. [docs/loans/PRODUCT_RULES.md](docs/loans/PRODUCT_RULES.md): what the loan product does, in plain terms.
2. [docs/adr/](docs/adr/): the decisions and their trade-offs.
3. [services/ledger/postgres/post.go](services/ledger/postgres/post.go): the posting transaction.
4. [services/lending/app/posting.go](services/lending/app/posting.go): how lending moves money without losing or repeating it.
5. [services/lending/app/payout_driver.go](services/lending/app/payout_driver.go): how an external payment with an unknown outcome is handled.
6. [services/lending/app/external_repayment.go](services/lending/app/external_repayment.go): why a webhook is never believed.
7. [services/lending/providers/paystack/](services/lending/providers/paystack/doc.go): a production adapter, and where the HTTP call is made.
8. [docs/TRACEABILITY.md](docs/TRACEABILITY.md): plan requirement → code → test → evidence.
