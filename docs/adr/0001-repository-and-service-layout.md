# 0001: Monorepo; three services for the first product

**Context.** The plan names nine launch services. Personal loans need identity (who is the customer, are they verified), a ledger (where the money is), and lending.

**Decision.** One Go module with three deployables: `ledgerd`, `identityd`, `lendingd`. Each service has its own directory, database, migrations and database roles. Shared code lives in `platform/` and is limited to things with no business meaning of their own (money arithmetic, the business calendar, outbox, Kafka and gRPC plumbing, token verification, telemetry).

**Rules that keep the boundaries real.**
- No service imports another service's `domain`, `app` or `postgres` package. The only cross-service imports are generated protobuf code, a service's `contract` package (names and error reasons), and `<service>test` kits from test code.
- No service reads or writes another's database.
- The ledger calls no one.

**Not built yet**, and deliberately so: gateway, payments, deposits, risk, recon, notify, cards, pos. They arrive with the products that need them.

**Consequence.** Fraud screening, payout and reconciliation logic that the plan assigns to `risk`, `payments` and `recon` currently sit in lending behind interfaces (see 0005). Moving them later is a change of adapter, not of use case.
