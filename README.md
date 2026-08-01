# Go gRPC Microservices Boilerplate

A production-shaped starting point for gRPC services in Go — where every architectural
decision is written down with its trade-off, and **proven by a test rather than a claim in
a README**.

Clone it, run one command, and you have a working service. No `make`, no `protoc`, no
`buf`, no Docker, no C compiler.

```bash
git clone <this-repo> && cd boilerplate-go-grpc-microservices
go tool task verify   # green in ~3s on a cold test cache
go run ./cmd/orderd   # gRPC on :50051, admin on :9090
```

---

## Status

This template is being built milestone by milestone. **What is here works and is tested;
what is not here is listed as not here.** A boilerplate that overstates itself wastes more
time than one that ships less.

| Area | Status | Notes |
|---|---|---|
| Proto toolchain (buf, no protoc) | ✅ Done | Codegen works offline, on Windows, with only Go installed |
| Domain layer (`internal/order`) | ✅ Done | Money, status machine, keyset pagination, shared store contract |
| gRPC transport + health + reflection | ✅ Done | Hardened server options, graceful drain |
| In-memory store (`STORE_DRIVER=memory`) | ✅ Done | Same contract the Postgres adapter will satisfy |
| Error model (`apperr`) | ✅ Done | One `Kind → gRPC code → HTTP status` table |
| bufconn test harness | ✅ Done | Tests drive the **production** server |
| Repo/import/toolchain guard tests | ✅ Done | CRLF, import boundaries, build-tag tiers, tool pins, Taskfile portability |
| CI (Linux + Windows), golangci-lint | ✅ Done | Lint is at 0 issues; `-race` runs on Linux |
| Full interceptor chain + observability | ⬜ M3 | Metrics, tracing, logging, admission, validation |
| Postgres via GORM + goose migrations | ⬜ M4 | `STORE_DRIVER=postgres` currently returns an explicit error |
| Real auth (OIDC/JWKS) + default-deny policy | ⬜ M5 | **See the security note below** |
| REST/JSON edge (grpc-gateway) | ⬜ M6 | `.pb.gw.go` is generated but not yet served |
| Redis cache-aside + rate limiting | ⬜ M7 | |
| Outbox → NATS JetStream → worker | ⬜ M8 | The `EventPublisher` port exists and is tested |
| Second service + inter-service hop | ⬜ M9 | |
| Dockerfile, compose, kustomize, e2e | ⬜ M10 | |
| `cmd/rename`, `cmd/scaffold`, ADRs | ⬜ M11 | |

> [!WARNING]
> **There is no real authentication yet.** `internal/platform/interceptor/devauth.go`
> injects a fixed principal without verifying anything. It is guarded three ways
> (`config.Validate` refuses `AUTH_MODE=dev` when `APP_ENV=production`, a WARN is logged on
> every startup, and the deploy overlay will set `AUTH_MODE=oidc` explicitly) — but until
> M5 lands, do not expose this to a network you do not control.

---

## Quickstart

**Requirements:** Go 1.26+. That is the entire list.

```bash
go tool task verify
```

Runs `go build`, `go vet`, and the full default test tier. It passes with the Docker daemon
stopped, no network (once the module cache is warm), and no C compiler.

```bash
go run ./cmd/orderd
```

Boots with an in-memory store seeded with three orders, so the first call returns data
instead of an empty page.

| Listener | Address | Purpose |
|---|---|---|
| gRPC | `:50051` | The API. Reflection and `grpc.health.v1` are registered |
| Admin | `127.0.0.1:9090` | `/healthz` now; `/metrics` and `/debug/pprof` in M3. **Bound privately on purpose** |
| Gateway | `:8080` | REST/JSON — arrives in M6 |

Poke it (install [grpcurl](https://github.com/fullstorydev/grpcurl) first — reflection means
you need no `.proto` file):

```bash
grpcurl -plaintext localhost:50051 list
```

```bash
grpcurl -plaintext -d '{}' localhost:50051 order.v1.OrderService/ListOrders
```

Other tasks — run `go tool task` to see them all:

| Command | What it does |
|---|---|
| `go tool task gen` | Regenerate protobuf code (no protoc needed) |
| `go tool task proto:lint` | Lint `.proto` files |
| `go tool task doctor` | Report what this machine can run, and why it can't run the rest |
| `go tool task test:race` | Race detector, or a clear explanation if cgo is unavailable |
| `go tool task lint` | golangci-lint v2 |

---

## Why this template

Most Go microservice templates fail in one of two directions: they are a toy that ignores
everything hard, or they are a framework so abstracted that nobody can read them. This one
optimises for two properties instead.

**1. A stranger gets green on an unknown machine.** `git clone` → `go test ./...` passes
with nothing installed but Go. No protoc (buf compiles protos in pure Go), no make (Windows
has none), no Docker for the default tier, no cgo.

**2. Every decision has a test that fails when it is violated.** A comment saying "the
domain must not import gRPC" is a wish. A test that walks the import graph and fails the
build is a guarantee. Examples already in the tree:

| Guarantee | Enforced by |
|---|---|
| Money never touches floating point | `money_test.go` walks the AST and fails on `float64` |
| The in-memory store behaves like the real database | One contract suite runs against both |
| The order and its event commit together or not at all | Contract asserts a rolled-back tx leaves neither |
| A cross-tenant read is indistinguishable from a miss | Contract asserts `ErrNotFound`, never `PermissionDenied` |
| The documented first-run experience actually works | `test/quickstart_test.go` executes it |

---

## Repository layout

```
proto/                      .proto sources
  order/v1/order.proto      the example domain
  third_party/              VENDORED deps -> codegen works offline
gen/go/                     generated code, committed. Never hand-edited.

cmd/
  orderd/                   the service. ~15 lines; everything lives in internal/app
  devtool/                  cross-platform task helpers (Taskfile can't do filesystem work)

internal/
  order/                    DOMAIN. Imports no gen/, no driver, no gRPC, no telemetry
    ordermem/                 in-memory store — also backs STORE_DRIVER=memory
    ordertest/                builders + the shared store contract
  grpcapi/                  THE proto boundary. convert / errmap / server / chain
  app/                      composition root: New, Run, Close, shutdown sequencer
  platform/                 cross-cutting, service-agnostic
    config/                   the ONLY package that reads the environment
    apperr/                   Kind enum + the one Kind->code->HTTP table
    auth/                     Principal and its context plumbing
    interceptor/              recovery, errmap, dev auth
    observability/            logging (metrics + tracing in M3)
  testutil/                 bufconn harness that boots the real server

test/                       cross-cutting guards only, never business tests
```

---

## Architecture

Three layers and one rule: **dependencies point inward, and the domain points at nothing.**

```mermaid
graph TD
    A["cmd/orderd<br/><i>~15 lines</i>"] --> B["internal/app<br/><i>composition root</i>"]
    B --> C["internal/grpcapi<br/><i>proto boundary</i>"]
    B --> D["internal/platform/*<br/><i>cross-cutting</i>"]
    C --> E["internal/order<br/><b>DOMAIN</b>"]
    C --> F["gen/go<br/><i>protobuf</i>"]
    E -.->|"implemented by"| G["ordermem<br/><i>in-memory</i>"]
    E -.->|"implemented by"| H["gormstore<br/><i>Postgres — M4</i>"]

    style E fill:#2d5016,stroke:#4a7c22,color:#fff
    style H stroke-dasharray: 5 5
```

`internal/order` imports only the standard library and `google/uuid`. It has never heard of
protobuf, gRPC, GORM, or OpenTelemetry.

**Why this is worth the mapping code in `convert.go`:** it means a proto field rename is a
change to one file instead of a database migration, and it means the business tests run in
milliseconds with no Docker. Once `*pb.Order` reaches your SQL layer, undoing that is a
simultaneous rewrite of every layer — this is the one boundary that is expensive to add
later and cheap to keep from day one.

The domain defines **ports**; adapters implement them:

```go
type Store interface {
    Create(ctx context.Context, o Order) error
    Get(ctx context.Context, tenantID, orderID string) (Order, error)
    List(ctx context.Context, tenantID string, f ListFilter) (Page, error)
    Update(ctx context.Context, o Order) error
}

// The callback receives the DOMAIN interfaces, never *gorm.DB.
// The publisher is passed alongside the store because the outbox row MUST be
// written by the same transaction as the business change.
type Atomic interface {
    InTx(ctx context.Context, fn func(Store, EventPublisher) error) error
}
```

Hand a driver type to that callback instead and three things break at once: the domain has
to import the driver, the in-memory fake can no longer implement it, and every transactional
business test silently becomes a Docker test.

### Request path

```
client → recovery → auth → errmap → OrderServer → order.Service → Store
```

`errmap` is **innermost** on purpose. It runs last on the way in and first on the way out,
so every interceptor outside it — logging, metrics, tracing in M3 — observes the *final*
status code. Put it anywhere else and your dashboards fill with `codes.Unknown` for errors
you carefully classified.

---

## Testing

Tiers are separated by **build tags, never `testing.Short()`**. A `Short()` skip still links
testcontainers into every test binary; a build tag makes the default tier's dependency set a
compile-time guarantee. That is the only way `go test ./...` is safe with Docker stopped.

| Tier | Command | Infra | Measured on this machine |
|---|---|---|---|
| Default | `go tool task verify` | none | **10.5s** cold cache, **0.9s** cached |
| Codegen | `go tool task verify:codegen` | network | **17.6s** — regenerates and byte-compares |
| Integration *(M4)* | `go tool task verify:int` | Docker | — |
| End-to-end *(M10)* | `go tool task verify:e2e` | Docker + compose | — |

Current: **42 test functions across 13 files**, many table-driven with a dozen or more cases.

### Guard tests

`test/` and `tools/` hold tests that assert nothing about business behaviour — they exist to
stop the template rotting. Each has been verified to actually **fail** when violated, not
merely to pass today:

| Guard | Catches |
|---|---|
| `TestNoCRLFInTrackedFiles` | Line endings that would break codegen diffs and golden files |
| `TestDomainImportsNothingHeavy` | The domain reaching for gRPC, GORM, protobuf, OTel |
| `TestPlatformDoesNotImportServices` | `platform/` becoming order-service helpers |
| `TestOnlyConfigReadsTheEnvironment` | A stray `os.Getenv` three layers down |
| `TestBannedToolsAreNotToolDependencies` | buf/golangci-lint entering the `tool` directive |
| `TestBufVersionIsConsistent` | The three buf pins drifting apart |
| `TestNoForbiddenShellCommands` | A Taskfile line that only works on Unix |
| `TestFilePathsAreQuoted` | Unquoted paths breaking on `C:\Program Files\...` |
| `TestDefaultTierNeedsNoDocker` | Docker deps leaking into the default tier |
| `TestTaggedTestsHaveAValidConstraint` | A `//go:build` missing its blank line, silently ignored |
| `TestSynctestIsNotUsedWithRealNetworking` | A synctest bubble that would hang forever |
| `TestGeneratedCodeIsUpToDate` | Committed `gen/` drifting from the `.proto` |
| `TestAllProtoFieldsAreAcknowledged` | A new proto field silently never mapped |

### The pattern worth stealing: one contract, two implementations

`ordertest.RunStoreContract` is a single suite of 15 behaviours. It runs unchanged against
the in-memory store (microseconds, no Docker) and — from M4 — against real Postgres in
testcontainers.

That is what makes the fast tier *trustworthy*. When a business test uses the in-memory
store and passes, it passes against behaviour real Postgres has also been shown to have. A
hand-written fake that nothing holds to a contract is just a second implementation of your
bugs — and unlike a generated mock, a fake can be *proven* equivalent to the real thing.

Assertions only Postgres can make (SQLSTATE mapping, `FOR UPDATE SKIP LOCKED` disjointness,
index usage) live in the adapter's own test file, not in the shared contract.

---

## Key decisions

Each links to where it lives. The full trade-off analysis for every one of these is in the
implementation plan; the short version:

| Decision | Choice | Why not the alternative |
|---|---|---|
| Transport | grpc-go + grpc-gateway | Connect-RPC's one-port win evaporates once pprof/metrics need a private listener, and it invalidates the entire go-grpc-middleware / otelgrpc / bufconn ecosystem |
| Proto codegen | [buf](buf.gen.yaml) via `go run …@v1.72.0` | **Never** the `tool` directive — buf's own docs forbid it, since MVS would silently move your protobuf *runtime* to satisfy a build tool |
| Codegen plugins | [`tool` directive](go.mod) | Here it is exactly right: sharing a module graph with `google.golang.org/protobuf` makes generator/runtime skew structurally impossible |
| Third-party protos | [vendored](proto/third_party/) | BSR deps resolve into a cache `go mod download` never warms, which would make "generate offline" false |
| Domain boundary | [mapping layer](internal/grpcapi/convert.go) | Passing `*pb.Order` to storage makes a field rename a migration |
| Money | [integer units + nanos](internal/order/money.go) | `float64` cannot represent `0.10`; the drift reconciles fine in tests and wrongly at month end |
| IDs | UUIDv7 | v4 is uniformly random, so every insert lands in a random B-tree page. Trade-off: v7 leaks creation time |
| Pagination | [keyset](internal/order/cursor.go) | `OFFSET` skips and repeats rows when data changes mid-page. Retrofitting keyset is a breaking API change **plus** an index change |
| Wiring | [manual constructors](internal/app/app.go) | A missing dependency is a compile error, not a runtime one. Adding fx/wire later is additive; removing either is a rewrite. `google/wire` is archived |
| Time in tests | `testing/synctest` | No `Clock` interface — the stdlib made that abstraction unnecessary in Go 1.25 |
| Assertions | stdlib + `go-cmp`/`protocmp` | One assertion idiom, not two. Adding testify later is purely additive |
| Task runner | [Taskfile](Taskfile.yml) via `go tool` | No Makefile: Windows has no `make`. Task's shell has no `rm`/`sed`/`jq` either, so those live in `cmd/devtool` |

---

## Configuration

Everything is environment variables, read in exactly one place
([`internal/platform/config`](internal/platform/config/config.go)) and validated **before
any listener binds**. Validation reports *every* problem at once — one at a time turns a
misconfigured deploy into five rollout attempts.

| Variable | Default | Notes |
|---|---|---|
| `APP_ENV` | `development` | `development` \| `staging` \| `production` |
| `GRPC_ADDR` | `:50051` | |
| `ADMIN_ADDR` | `127.0.0.1:9090` | Keep it private — it serves pprof from M3 |
| `STORE_DRIVER` | `memory` | `memory` \| `postgres` (M4) |
| `AUTH_MODE` | `dev` | **Refused when `APP_ENV=production`** |
| `POSTGRES_MAX_OPEN_CONNS` | `10` | Explicit on purpose: pool defaults read the *node's* CPU count, not the cgroup limit, so 20 replicas exhaust `max_connections` |
| `GRPC_MAX_CONCURRENT_STREAMS` | `250` | grpc-go's default is effectively unbounded — this is a DoS control |
| `GRPC_MAX_CONNECTION_AGE` | `30m` | Forces periodic GOAWAY so rolling deploys actually rebalance |
| `SHUTDOWN_DRAIN_DELAY` | `5s` | Health flips to NOT_SERVING, then waits. Kubernetes deregisters asynchronously, so a pod that stops instantly still gets traffic |

Secrets use a [`Secret` type](internal/platform/config/config.go) that redacts through
`String`, `MarshalJSON`, **and** `LogValue` — the three routes by which a password normally
escapes. Covering only one is the usual mistake.

---

## Toolchain notes

**No `protoc`.** buf compiles `.proto` files with a pure-Go compiler. `go tool task gen`
works on a machine that has only Go.

**No `make`.** Windows does not ship it. [`Taskfile.yml`](Taskfile.yml) commands may invoke
only `go`, `docker`, `kubectl`, and `git`, because Task's embedded shell has no `rm`, `cp`,
`sed`, `awk`, or `jq` builtins on Windows. Anything filesystem- or text-shaped lives in
[`cmd/devtool`](cmd/devtool/main.go) in Go.

**`go test -race` will not run on stock Windows.** The race detector links the C++
ThreadSanitizer runtime, so it requires cgo and therefore a C compiler. This is a permanent
limitation, not a configuration gap. `go tool task test:race` detects it and prints the two
real options (a container, or WinLibs + `CGO_ENABLED=1`) instead of a bare
`-race requires cgo`. CI runs `-race` on Linux.

**`.gitattributes` is the first commit in this repo's history**, before any generated file
existed. With `core.autocrlf=true` — a common Windows default — git checks out CRLF while
every generator writes LF, which silently breaks the codegen diff test, golden files, and
`gofmt` in CI, for a reason a newcomer cannot diagnose.

**Docker Desktop + testcontainers:** testcontainers resolves the daemon via
`~/.testcontainers.properties`, `DOCKER_HOST`, then the *default* named pipe — it does not
read Docker contexts. On Docker Desktop the active context is usually `desktop-linux`, so a
healthy daemon can be invisible. `go tool task doctor` detects and explains this.

---

## Making it yours

The module path is the placeholder `github.com/example/gomicro`.

`cmd/rename` (M11) will rewrite `go.mod`, every import, `buf.gen.yaml`'s
`go_package_prefix`, and the image references in one command, then delete itself. Until it
lands, change `go.mod` and the `go_package_prefix` in [`buf.gen.yaml`](buf.gen.yaml), then
run `go tool task gen`.

Because managed mode owns `go_package`, **no `.proto` file hard-codes an import path** —
forking is one line in `buf.gen.yaml`, not one line per proto.

### Adding an RPC

1. Add it to [`proto/order/v1/order.proto`](proto/order/v1/order.proto) with a
   `google.api.http` binding and protovalidate rules.
2. `go tool task gen`
3. Implement the business rule in `internal/order` and add a case to the store contract if
   it touches persistence.
4. Add the thin adapter method in [`internal/grpcapi/server.go`](internal/grpcapi/server.go).

From M5, step 4 will not compile until you add an explicit authorization entry — a new RPC
physically cannot ship without a conscious auth decision.

---

## Roadmap

M2 toolchain guard tests · M3 interceptors + observability · M4 Postgres/GORM/goose ·
M5 OIDC auth + default-deny policy · M6 REST edge · M7 Redis · M8 outbox + JetStream ·
M9 second service · M10 deploy + e2e · M11 rename/scaffold/ADRs

Each milestone lands on its own branch and ends with a green `go tool task verify`.
