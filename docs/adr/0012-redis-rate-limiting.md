# 0012: Redis holds rate-limit counters and nothing else; the limiter fails open

**Decision.** Sign-in attempts (per phone or email) and loan-application submissions (per customer) are counted in Redis with a fixed-window counter (one Lua script: `INCR`, `PEXPIRE` on first hit, `PTTL`). Subjects are hashed in keys. Each call has a 150 ms deadline and no client retries.

**Why Redis here and nowhere else.** A rate-limit counter is high-frequency, short-lived and worthless after its window. Losing it costs a little abuse protection for a minute. That is exactly the data Redis is good for and exactly the data PostgreSQL should not be asked to churn. Everything that must be right lives in PostgreSQL.

**Failure policy: fail open.** If Redis is unreachable or slow the request proceeds, the event is logged, and `ratelimit_decisions_total{outcome="unavailable"}` rises (alert `RateLimiterUnavailable`).

This is safe only because Redis is not the control that protects anything:

| Risk | Real control (still enforced with Redis down) |
|---|---|
| PIN guessing | Lockout counter under a row lock in PostgreSQL |
| Duplicate applications | Partial unique index: one open application per customer and product |
| Application flooding | Velocity rule in fraud screening, counted in PostgreSQL |
| Duplicate money movement | Idempotency keys and ledger posting references in PostgreSQL |

Failing closed would turn a cache outage into a sign-in outage for every customer.

**Why fixed window.** One round trip, one small key, atomic. Its weakness (up to twice the limit across a window boundary) is acceptable for abuse protection and limits are set with it in mind. A sliding log or token bucket is the upgrade if a limit ever needs to be exact.

**Not done.** Per-IP limits need the gateway (the client address is not trustworthy behind a proxy we do not yet have). Registration is not rate limited for the same reason.

**Tested.** `platform/ratelimit` against real Redis (limit, window reset, concurrency, outage); identity and lending with Redis up and with Redis down.
