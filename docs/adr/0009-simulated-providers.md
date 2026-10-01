# 0009: Simulated providers, one real adapter, and how production is protected

**Situation.** No provider credentials or sandbox access exist for this project. For BVN verification and credit bureaus there is also no public contract that could be verified. For Paystack there is: an official OpenAPI specification.

**Decision.**
- Each integration is a port with a documented contract (what an error means, what must be idempotent).
- A deterministic simulator ([simulator/](../../simulator/server.go)) stands in for every provider. It models no real provider's API; its contract is this project's own ([docs/contracts/provider-simulator.md](../contracts/provider-simulator.md)). Every simulator adapter makes a real HTTP call to it.
- Simulator adapters are named `*sim` and report themselves as `*-simulator` in records and metrics.
- **Where a public contract could be verified, a production adapter was written from it**: [`paystack`](../../services/lending/providers/paystack/doc.go) for name enquiry, transfers, transfer status and payment verification. It uses only what the specification states, treats everything the specification leaves undefined as "unknown", and is tested against a local server returning the specified shapes.
- **Where no contract could be verified, no production adapter was written** (BVN, credit bureau). An adapter against guessed endpoints would be worse than none.

**Guards.**
- `APP_ENV` must be `local` or `test` for any simulator adapter to be selected; otherwise startup fails with a message naming the setting.
- Simulator and production settings are separate variables (`PROVIDER_SIM_*` vs `PAYSTACK_*`); selecting `paystack` without `PAYSTACK_SECRET_KEY` fails startup.
- The Paystack adapter refuses a non-HTTPS base URL unless it is loopback.
- Paystack webhooks, whose signing scheme is not in the specification, are off unless `PAYSTACK_WEBHOOKS_ENABLED=true`.
- Outside local and test the illustrative product and policy files are also refused.
- `devsetup`, `providersim`, `ledgerd devseed` and `identityd create-staff` refuse to run elsewhere.

**What the simulator proves.** That the platform behaves correctly for each outcome class: success, rejection, timeout, success with a lost response, delayed success, malformed response, duplicate, forged and out-of-order notifications, and a settlement report that disagrees.

**What the Paystack contract tests prove.** That the adapter sends the documented requests and maps the documented responses as intended.

**What neither proves.** Anything about a real provider's behaviour: latency, undocumented codes, whether a send is really idempotent, whether a report is final. The Paystack adapter has never been run against Paystack; a sandbox certification run is a launch dependency ([docs/integrations/README.md](../integrations/README.md)).
