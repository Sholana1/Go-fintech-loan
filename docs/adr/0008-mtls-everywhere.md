# 0008: Mutual TLS in every environment, including tests

**Decision.** `platform/grpcx` refuses to build a server without verified client certificates. There is no insecure mode. Tests and local development use short-lived certificates from `platform/devcert`.

**Why.** The ledger authorises on the caller's workload identity. If that path is switched off in tests, the tests exercise a different program from the one that runs in production.

**Identity** is a SPIFFE-style URI SAN (`spiffe://bankplatform.internal/svc/<name>`).

**Open.** The production certificate authority and rotation mechanism are not chosen. **Launch dependency.**
