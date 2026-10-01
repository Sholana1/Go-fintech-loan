# 0007: Lending depends on its concrete PostgreSQL store

**Decision.** `lending/app` uses `*postgres.Store` directly. External systems (ledger, identity, bureau, payout, fraud) are interfaces.

**Why.** There is one persistence implementation and the tests run against real PostgreSQL, so an interface would have no second implementation and would hide which statements run in which transaction. External systems do have several implementations (real adapter, simulator client, fault-injecting wrapper).

**How business logic stays out of SQL.** Rules are pure functions in `domain`. `app` opens a transaction, calls `postgres.Queries` methods and `domain` functions in order, and commits. Each `Queries` method is one statement.

**Cost.** Use cases cannot be unit-tested without a database. Accepted: the properties that matter (locking, uniqueness, compare-and-set) only exist in the database.
