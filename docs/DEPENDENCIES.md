# Dependencies

Go 1.27. Versions are pinned in [go.mod](../go.mod) and were current when selected (1 October 2026). Each is the library's stable release line.

| Dependency | Version | Used for | Why this one |
|---|---|---|---|
| `github.com/jackc/pgx/v5` | 5.11.0 | PostgreSQL driver and pool | Native protocol, explicit transactions, typed errors with SQLSTATE and constraint names |
| `github.com/pressly/goose/v3` | 3.28.0 | Schema migrations | Plain SQL files embedded in the binary; no DSL |
| `google.golang.org/grpc` | 1.84.0 | Internal service calls | Deadlines, mTLS, status codes with details |
| `google.golang.org/protobuf` | 1.36.12 | Generated message code | — |
| `google.golang.org/genproto/googleapis/rpc` | 2026-09-28 | `ErrorInfo` details on gRPC errors | Stable reasons without parsing messages |
| `github.com/twmb/franz-go` (+ `kadm`) | 1.22.1 / 1.19.0 | Kafka producer, consumer, topic admin | Pure Go, idempotent producer, manual offset commits |
| `go.opentelemetry.io/otel` and SDKs | 1.46.0 | Traces and metrics | Vendor-neutral instrumentation |
| `…/otelgrpc`, `…/otelhttp` | 0.71.0 | Automatic gRPC and HTTP spans | — |
| `…/exporters/prometheus` | 0.68.0 | Expose metrics for scraping | — |
| `github.com/prometheus/client_golang` | 1.24.1 | Registry and `/metrics` handler | — |
| `github.com/redis/go-redis/v9` | 9.22.0 | Rate-limit counters | The maintained Redis client; Lua scripting and per-call deadlines |
| `github.com/golang-jwt/jwt/v5` | 5.3.1 | Access tokens (EdDSA only) | Algorithm allow-list, required-expiry option |
| `golang.org/x/crypto` | 0.57.0 | argon2id | — |
| `github.com/google/uuid` | 1.6.0 | Identifiers | — |

## Build and analysis tools (installed by `make tools` into `.tools/`)

| Tool | Version |
|---|---|
| buf | 1.73.0 |
| protoc-gen-go | 1.36.12 |
| protoc-gen-go-grpc | 1.6.2 |
| staticcheck | 2026.2.1 |
| govulncheck | latest at install |

## Local infrastructure images

| Image | Purpose |
|---|---|
| `postgres:16-alpine` | PostgreSQL |
| `apache/kafka:3.9.0` | Kafka, single node, KRaft |
| `prom/prometheus:v3.5.0` | Optional, `--profile obs` |

## Deliberately not used

| Not used | Reason |
|---|---|
| ORM or query builder | Locking and constraint behaviour must be visible in the SQL |
| Dependency-injection framework | Constructors in `main` are explicit and short |
| Workflow engine | Two-step sagas are handled by posting intents (ADR 0004) |
| Service mesh | mTLS and identity are in `platform/grpcx`; nothing else is needed yet |
| Redis, SQS | No workload needs them yet (ADR 0006) |
| Float types for money | Integer minor units and exact rationals throughout |

## Known vulnerability exceptions

See [security/vuln-exceptions.txt](../security/vuln-exceptions.txt). One entry: `GO-2026-6443` in gRPC 1.84.0, no tagged fix available, expires 15 November 2026.
