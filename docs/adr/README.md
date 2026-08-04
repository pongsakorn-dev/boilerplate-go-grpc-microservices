# Architecture decisions

**The reasoning for a decision lives next to the thing it decided.** This directory is an index
into that, plus the few decisions that have nowhere else to live.

That is a deliberate departure from the usual `0001-…md` … `0018-…md`, and
[ADR 0001](0001-decisions-live-next-to-the-code.md) is the argument for it. The short version: a
decision document that restates what a doc comment already says is a second copy, second copies
drift, and the drifted one is the one a stranger reads first.

---

## The index

Every locked decision, and where its reasoning actually is. If a link here goes stale,
`test/docs_test.go` fails.

| # | Decision | Where the argument lives |
|---|---|---|
| 1 | Scope: a platform starter, not a product | [README — Why this template](../../README.md) |
| 2 | grpc-go + grpc-gateway, JSON edge in client mode | [`internal/gateway/`](../../internal/gateway/) — "client mode, and why it is the whole design" |
| 3 | One `go.mod`, `gen/` committed | [`buf.gen.yaml`](../../buf.gen.yaml) and [README — Toolchain notes](../../README.md) |
| 4 | Domain never imports `gen/`, GORM, gRPC or OTel | [`test/layout_test.go`](../../test/layout_test.go) — the rule is the test; [ADR 0005](0005-ports-and-adapters-without-the-ceremony.md) for the shape it implies |
| 5 | GORM over sqlc, and what replaces what it gives up | [README — Persistence](../../README.md); [`internal/platform/gormx/`](../../internal/platform/gormx/) |
| 6 | goose SQL migrations, never run on boot | [`cmd/migrate/`](../../cmd/migrate/); [README — Migrations never run on boot](../../README.md) |
| 7 | `tenant_id` column + a fail-closed GORM callback | [`internal/platform/gormx/tenant.go`](../../internal/platform/gormx/tenant.go) |
| 8 | Idempotency keys — **cut** | [ADR 0002](0002-what-was-cut.md) |
| 9 | Redis for quotas only; cache-aside **cut** | [`internal/platform/config/config.go`](../../internal/platform/config/config.go) — `RedisConfig`; [ADR 0002](0002-what-was-cut.md) |
| 10 | Outbox → JetStream → worker, at-least-once | [`internal/platform/outbox/outbox.go`](../../internal/platform/outbox/outbox.go) — the guarantees table; [`internal/platform/events/events.go`](../../internal/platform/events/events.go) |
| 11 | Manual constructor wiring in a composition root | [`internal/app/app.go`](../../internal/app/app.go) |
| 12 | `AUTH_MODE=dev\|oidc`, JWKS verification | [`internal/platform/auth/verifier.go`](../../internal/platform/auth/verifier.go); [`internal/platform/auth/jwks.go`](../../internal/platform/auth/jwks.go) |
| 13 | Default-deny authorisation, driven from `protoregistry` | [`internal/platform/auth/policy.go`](../../internal/platform/auth/policy.go); [`internal/grpcapi/policy.go`](../../internal/grpcapi/policy.go) |
| 14 | Money as currency + units + nanos, never `float64` | [`internal/order/money.go`](../../internal/order/money.go); [`internal/platform/migrations/00001_orders.sql`](../../internal/platform/migrations/00001_orders.sql) |
| 15 | Prometheus + OTel traces + `log/slog` | [`internal/platform/observability/`](../../internal/platform/observability/) |
| 16 | `cmd/rename` kept, `cmd/scaffold` **cut** | [ADR 0002](0002-what-was-cut.md); [ADR 0004](0004-generated-code-is-regenerated-not-rewritten.md) |
| 17 | kustomize base + one dev overlay | [`deploy/k8s/overlays/dev/kustomization.yaml`](../../deploy/k8s/overlays/dev/kustomization.yaml) — "one overlay ships, not three" |
| 18 | The repository does not live in a synchronising folder | [ADR 0003](0003-the-repo-does-not-live-in-a-sync-folder.md) |

Beyond the numbered set, the arguments worth reading before forking:

| Topic | Where |
|---|---|
| Interceptor **order**, and why each position is load-bearing | [`internal/grpcapi/chain.go`](../../internal/grpcapi/chain.go) |
| Never leak, never lose: the error model | [`internal/platform/apperr/apperr.go`](../../internal/platform/apperr/apperr.go) |
| Why the caller's token is never forwarded | [`internal/platform/client/identity.go`](../../internal/platform/client/identity.go) |
| Why the tenant is not in a NATS subject | [`internal/platform/events/events.go`](../../internal/platform/events/events.go) |
| Why generated code is regenerated, not rewritten | [ADR 0004](0004-generated-code-is-regenerated-not-rewritten.md) |
| Why this is hexagonal without saying so, and what was dropped | [ADR 0005](0005-ports-and-adapters-without-the-ceremony.md) |
| What you can delete, in an order that works | [`docs/DELETING.md`](../DELETING.md) |

---

## The ADRs

Only for decisions with no code to sit next to.

| | |
|---|---|
| [0001](0001-decisions-live-next-to-the-code.md) | Decisions live next to the code |
| [0002](0002-what-was-cut.md) | What was cut, and the test every cut had to pass |
| [0003](0003-the-repo-does-not-live-in-a-sync-folder.md) | The repository does not live in a synchronising folder |
| [0004](0004-generated-code-is-regenerated-not-rewritten.md) | Generated protobuf code is regenerated, never rewritten |
| [0005](0005-ports-and-adapters-without-the-ceremony.md) | Ports and adapters, without the ceremony |
