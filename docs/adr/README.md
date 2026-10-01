# Architecture decision records

Decisions made while implementing the plan, including every place the implementation departs from it.

| # | Decision | Status |
|---|---|---|
| [0001](0001-repository-and-service-layout.md) | Monorepo; three services for the first product | Accepted |
| [0002](0002-rest-per-service-gateway-deferred.md) | Each service exposes its own REST surface; gateway deferred | Accepted, departs from plan |
| [0003](0003-ledger-invariants.md) | Which mechanism enforces each ledger invariant | Accepted |
| [0004](0004-posting-intents.md) | Posting intents for every lending→ledger movement | Accepted |
| [0005](0005-external-payout-inside-lending.md) | Loan payout and reconciliation live in lending for now | Accepted, departs from plan |
| [0006](0006-kafka-sqs-redis.md) | Kafka introduced; Redis for rate limiting only; SQS not yet | Accepted |
| [0007](0007-persistence-not-abstracted.md) | Lending depends on its concrete PostgreSQL store | Accepted |
| [0008](0008-mtls-everywhere.md) | Mutual TLS in every environment, including tests | Accepted |
| [0009](0009-simulated-providers.md) | Simulated providers, one real adapter, and how production is protected | Accepted |
| [0010](0010-hot-accounts-deferred.md) | System-account striping deferred, with measurements | Accepted |
| [0011](0011-external-repayments.md) | External repayments are verified with the provider and credited to the deposit account first | Accepted |
| [0012](0012-redis-rate-limiting.md) | Redis holds rate-limit counters only; the limiter fails open | Accepted |
