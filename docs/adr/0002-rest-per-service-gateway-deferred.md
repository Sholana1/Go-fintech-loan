# 0002: Each service exposes its own REST surface; gateway deferred

**Plan.** A `gateway` service terminates client traffic, authenticates, rate-limits and routes.

**Decision.** For the first product, `identityd` and `lendingd` each serve their own REST API and verify access tokens themselves. Identity signs tokens with an Ed25519 private key; other services hold only the public key.

**Why.** A gateway that only proxies adds a hop and a deployable without adding a control. Its real jobs (edge rate limiting, WAF, routing) need infrastructure decisions that have not been made.

**What is missing because of this.** Edge rate limiting. Credential guessing is bounded by a lockout counter in PostgreSQL (tested under concurrency), and duplicate applications by a unique index, so the money-relevant abuse cases are covered; volumetric abuse is not. **Launch dependency.**

**Revisit** when a second client-facing product exists or when edge infrastructure is chosen.
