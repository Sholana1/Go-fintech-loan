# 0006: Kafka introduced; Redis for rate limiting only; SQS not yet

**Kafka: introduced.** Role: the durable log of committed domain facts. The ledger publishes `ledger.journal.posted`; lending publishes loan lifecycle events. One consumer exists: lending reacts to money arriving for a customer with an overdue auto-debit loan. Delivery is at-least-once; the consumer deduplicates with an inbox row committed with its effect. No exactly-once claim is made beyond that boundary.

**SQS: not introduced.** The plan gives SQS single-owner background jobs. Personal loans have such jobs (status polling, posting retries, daily accrual, payment verification), but each is driven from a PostgreSQL work-queue column (`next_attempt_at`) claimed with a lease. That is already durable, needs no second source of truth for "what is pending", and survives a crash by construction. SQS earns its place when a workload needs fan-out to independent workers or a third party's delivery semantics (notifications are the likely first).

**Redis: introduced for one purpose, rate limiting** (sign-in attempts, loan-application submissions). See [0012](0012-redis-rate-limiting.md). It holds nothing that protects money, nothing that cannot be lost, and no cache: no caching need has been measured.

**Schema registry: not introduced.** Events are JSON with a version in the envelope; payload structs live in each producer's `contract` package. A registry with compatibility checks is a launch dependency once a second team consumes these topics.
