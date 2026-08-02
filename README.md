# Go gRPC Microservices Boilerplate

A production-shaped starting point for gRPC services in Go — where every architectural
decision is written down with its trade-off, and **proven by a test rather than a claim in
a README**.

Clone it, run one command, and you have a working service. No `make`, no `protoc`, no
`buf`, no Docker, no C compiler.

```bash
git clone git@github.com:pongsakorn-dev/boilerplate-go-grpc-microservices.git
cd boilerplate-go-grpc-microservices
go test ./...         # the whole default test tier -- no Docker, no protoc, no make
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
| LICENSE, SECURITY.md, third-party attribution | ✅ Done | Apache-2.0; `proto/third_party/` carries its own LICENSE + provenance |
| Repo/import/toolchain guard tests | ✅ Done | CRLF, import boundaries, build-tag tiers, tool pins, Taskfile portability |
| CI (Linux + Windows), golangci-lint | ✅ Done | Lint is at 0 issues; `-race` runs on Linux |
| Full interceptor chain | ✅ Done | recovery, metrics, logging, errmap, admission, auth, deadline, validation |
| Observability: slog + Prometheus + OTel traces | ✅ Done | Trace-correlated logs, `/metrics`, private pprof, OTLP export |
| Real auth (OIDC/JWKS) + default-deny policy | ✅ Done | Hostile-issuer suite, key rotation, revocation, default-deny policy the server refuses to boot without |
| Keycloak realm example | ⚠️ Partial | Structurally tested, **never booted against a real Keycloak** — see [`deploy/keycloak/`](deploy/keycloak/) |
| Postgres via GORM + goose migrations | ⬜ M4 · next | `STORE_DRIVER=postgres` returns an explicit error today |
| REST/JSON edge (grpc-gateway) | ⬜ M6 | `.pb.gw.go` is generated but not yet served |
| Distributed rate limiting (Redis) | ⬜ M7 | Scope **reduced**: cache-aside was cut |
| Outbox + relay | ⬜ M8a | The `EventPublisher` port exists and is tested |
| NATS JetStream + worker | ⬜ M8b | Deliberately **after** M10, so the deploy story exists first |
| Typed client + deadline budget | ⬜ M9 | Scope **reduced**: the second service was cut |
| Dockerfile, compose, kustomize, e2e | ⬜ M10 | Also where the Keycloak realm finally gets booted |
| `cmd/rename`, ADRs, `DELETING.md` | ⬜ M11 | Scope **reduced**: `cmd/scaffold` was cut |

> [!NOTE]
> **`AUTH_MODE=dev` is still the default, and it authenticates nobody.** That is what makes
> `git clone && go run ./cmd/orderd` work with no identity provider. It is refused when
> `APP_ENV=production`, it logs a warning on every startup, and `AUTH_MODE=oidc` is now a
> real JWT/JWKS verifier rather than a value that quietly did nothing. See
> [Authentication](#authentication) and [SECURITY.md](SECURITY.md).

---

## Quickstart

**Requirements:** Go 1.26+. That is the entire list.

```bash
go test ./...
```

That is the whole default tier. It passes with the Docker daemon stopped, no network once
the module cache is warm, and no C compiler.

### The task runner

Richer targets live in [`Taskfile.yml`](Taskfile.yml). go-task is deliberately **not** a
`go.mod` tool dependency. It declares requirements on grpc, protobuf and `x/net`, so it
would participate in module version selection for the runtime your service ships — the exact
pattern `test/toolpins_test.go` bans buf for. Removing it took `go.sum` from **352 modules to
114** and `go.mod` from **129 indirect requirements to 26**. Invoke it the way buf and
golangci-lint are:

```bash
go run github.com/go-task/task/v3/cmd/task@v3.52.0 --list-all
```

The rest of this README abbreviates that as `task`. Alias it, or just read `Taskfile.yml` —
every target is one or two plain `go` commands.

```bash
go run ./cmd/orderd
```

Boots with an in-memory store seeded with three orders, so the first call returns data
instead of an empty page.

| Listener | Address | Purpose |
|---|---|---|
| gRPC | `:50051` | The API. Reflection and `grpc.health.v1` are registered |
| Admin | `127.0.0.1:9090` | `/healthz`, `/metrics`, `/debug/pprof`. **Bound privately on purpose** |
| Gateway | — | REST/JSON arrives in M6, along with its `GATEWAY_ADDR` setting |

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
| `task gen` | Regenerate protobuf code (no protoc needed) |
| `task proto:lint` | Lint `.proto` files |
| `task doctor` | Report what this machine can run, and why it can't run the rest |
| `task test:race` | Race detector, or a clear explanation if cgo is unavailable |
| `task lint` | golangci-lint v2 |

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
    interceptor/              recovery, logging, errmap, admission, auth, deadline, validate
    observability/            slog+TraceHandler, Prometheus, OTel traces, admin mux
  testutil/                 bufconn harness that boots the real server

test/                       ALL cross-cutting guards, incl. proto toolchain and Taskfile
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
client
  -> recovery      panic containment
  -> metrics       counts shed load too, so it sits outside admission
  -> logging       observes the FINAL code, because errmap is below it
  -> errmap        domain error -> gRPC status + google.rpc details
  -> admission     concurrency limit, BEFORE auth
  -> auth          establishes the principal
  -> deadline      bounds the handler and everything downstream
  -> validate      protovalidate, from rules declared in the .proto
  -> OrderServer -> order.Service -> Store
```

**`errmap` is not innermost, and that placement is the subtle one.** "Innermost" is the
intuitive choice and it is wrong: an interceptor only maps errors from what it *wraps*.
Placed last it wraps the handler alone, so rejections from admission, auth and validation
sail past it unmapped and reach the client as `codes.Unknown` — and those are the errors
clients hit most often.

The real requirement is two-sided: below logging and metrics (so they record the mapped
code), and above every interceptor that produces an error (so those get mapped at all).
`internal/grpcapi/chain_test.go` caught this by asserting outcomes rather than reading the
list.

---

## Testing

Tiers are separated by **build tags, never `testing.Short()`**. A `Short()` skip still links
testcontainers into every test binary; a build tag makes the default tier's dependency set a
compile-time guarantee. That is the only way `go test ./...` is safe with Docker stopped.

| Tier | Command | Infra | Measured on this machine |
|---|---|---|---|
| Default | `go test ./...` | none | **8.2s** cold cache, **1.1s** cached |
| Codegen | `task verify:codegen` | network | **17.6s** — regenerates and byte-compares |
| Integration *(M4)* | `task verify:int` | Docker | — |
| End-to-end *(M10)* | not yet — arrives with the compose stack | Docker + compose | — |

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
| `TestEveryKindIsMapped` | A new error Kind with no gRPC/HTTP row |
| `TestInternalErrorsNeverLeakAndNeverLose` | Driver text reaching a client, or vanishing from the log |
| `TestSecretIsRedactedThroughEveryEscapeRoute` | A password escaping via fmt, JSON **or** slog |
| `TestValidateRejectsDevAuthInProduction` | `AUTH_MODE=dev` reaching production |
| `TestTraceHandlerPassesTheStdlibConformanceSuite` | A wrapping slog handler breaking `WithGroup` |
| `TestTraceHandlerSurvivesWith` | Derived loggers silently losing trace correlation |
| `TestPprofIsOnDefaultServeMuxButWeNeverServeIt` | Profiling endpoints on a public listener |
| `TestAdmissionReleasesSlotOnPanic` | A panicking handler permanently consuming a slot |
| `TestCommentsDoNotCiteMissingTests` | A comment claiming a proof that does not exist |
| `TestVendoredProtosKeepTheirLicenseHeaders` | Apache-2.0 attribution being stripped from vendored protos |
| `TestTaskTargetsReferenceRealPaths` | A task target pointing at a file that does not exist |
| `TestBannedToolsAreNotToolDependencies` | A build tool entering the production module graph |
| `TestErrorsAreMappedBeforeLoggingObservesThem` | Interceptor order regressing to `codes.Unknown` |

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

## Authentication

Two modes, selected by `AUTH_MODE`. The selection lives in one factory
([`auth.NewVerifier`](internal/platform/auth/verifier.go)) whose **default arm returns an
error** — never a permissive fallback, never a `nil` verifier.

That shape is not defensive styling; it is a fix for a bug this repo actually shipped. The
server once installed the dev interceptor unconditionally and never read `AUTH_MODE` at all,
so `APP_ENV=production AUTH_MODE=oidc` validated, booted clean, reported healthy, and served
every request as a full-scope `dev-tenant` principal. The bypass was confirmed by calling an
RPC with no credentials and getting three orders back. A factory that cannot say *"I do not
recognise this mode"* will fall through to whatever it can build, and what it can build is
always the permissive one.

| Mode | Verifies | Use |
|---|---|---|
| `dev` (default) | **Nothing.** Returns a fixed principal for any input, including no token at all | So a fresh clone runs with no identity provider. Refused when `APP_ENV=production`; warns on every startup |
| `oidc` | JWT signature against the issuer's JWKS, plus `iss`, `aud`, `exp`, `nbf`, `kid` | Everything else |

### What the verifier refuses

Each row is a token that verifies against *something*, which is what makes it dangerous.
All are in [`oidc_verifier_test.go`](internal/platform/auth/oidc_verifier_test.go).

| Rejected | Why it matters |
|---|---|
| `alg: none` | The original JWT vulnerability: an unsigned token |
| HS256 MAC'd with the issuer's **public** key | Algorithm confusion — the public key is published in the JWKS, so anyone can use it as an HMAC secret |
| A symmetric (`oct`) key in the JWKS | Publishing a symmetric key publishes the ability to mint tokens |
| RSA below 2048 bits | Factorable by well-resourced attackers |
| PS256 signed with a key published as RS256 | Algorithm substitution *within* a key type — the one case only our own check catches |
| Wrong `aud` | A token minted for a **different application on the same IdP** |
| No `exp` | A token with no expiry never expires |
| No tenant claim | Cryptographically perfect and useless in a multi-tenant service |
| A discovery document whose `issuer` disagrees | Stops a redirect or hijacked DNS pointing this service at a JWKS an attacker controls |

The suite runs against an **in-process issuer**
([`testjwks`](internal/platform/auth/testjwks/testjwks.go)) — no Docker, no network, no
provider. That is not only convenience: half these cases need a *hostile* issuer, and no real
provider will serve a symmetric key or go offline mid-request on request.

Key rotation, cache-hit counts, revocation and IdP outages are covered in
[`jwks_test.go`](internal/platform/auth/jwks_test.go). The outage rule is deliberately
three-way, because it is the one people get wrong in both directions: **no cached keys +
fetch fails → refuse; cached key hit + fetch fails → accept** (a signature does not become
invalid because the IdP is restarting); **unknown key + fetch fails → refuse.**

Two properties there are less obvious and were both added after an adversarial review pass
found them missing:

- **The key set has a maximum age** (`OIDC_MAX_KEY_AGE`, default 15m). Refetching only on a
  cache *miss* sounds sufficient and is not: when an issuer revokes a key it keeps signing
  with its others, so every legitimate token names a cached key, no miss ever occurs, and the
  revoked key stays trusted until the pod restarts.
- **The JWKS fetch happens with no reader lock held.** Otherwise one *unauthenticated*
  request naming an unknown key id — `kid` is read before any signature check — stalls every
  concurrent verification for the IdP's full response time, ignoring their deadlines, while
  holding admission slots.

### Authorisation is default-deny, and the server will not boot without it

[`DefaultPolicy`](internal/grpcapi/policy.go) maps every RPC to a rule. A method with no
entry is **denied** — and `NewServer` calls `Policy.ValidateCoverage` after registration, so
an uncovered method stops the process rather than waiting for someone to run the tests.

That check earned its place immediately. The policy was written from `order.proto` plus the
health methods from memory — `Check` and `Watch`. The server refused to start:
`/grpc.health.v1.Health/List` is registered by grpc-go, appears in no `.proto` here, and
would have been missed by any policy derived from the schema alone.

Coverage is asserted from two directions, because *"what does this service expose"* has two
different answers: `GetServiceInfo` knows about health and reflection but not about an RPC
that was generated and never wired up, and the descriptor registry knows the opposite. The
registry also needed filtering — it holds descriptors for everything **linked in**, and this
repo imports the OTLP exporter, so the naive version demanded an authorisation rule for the
OpenTelemetry collector.

### Providers

Nothing in `internal/platform/auth` knows what Keycloak is. Point it at any OIDC provider and
set two claim paths:

| Provider | `OIDC_TENANT_CLAIM` | `OIDC_SCOPE_CLAIM` |
|---|---|---|
| Keycloak | `tenant_id` | `scope` or `realm_access.roles` |
| Auth0 | `https://yourapp.example.com/tenant_id` | `scope` |
| Cognito | `custom:tenant_id` | `cognito:groups` |
| Entra ID | `tid` | `scp` |

A worked Keycloak realm is in [`deploy/keycloak/`](deploy/keycloak/) — including the
**audience mapper**, which is the single most common OIDC integration failure: a Keycloak
access token's default `aud` is `account`, not your API. That realm file has not yet been
booted against a real Keycloak; see the note in its README.

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

Settings for subsystems that do not exist yet (Redis, NATS, the gateway) are deliberately
**absent** rather than present-and-ignored — a config field that reads as wired and is not is
the same class of problem as a proto field that validates and is then dropped.

| `AUTH_MODE` | `dev` | `dev` \| `oidc`. **Refused when `APP_ENV=production`**; an unknown value refuses to start |
| `OIDC_ISSUER_URL` | | Required for `oidc`. Discovery finds the JWKS; `http://` is refused off loopback |
| `OIDC_AUDIENCE` | | Required for `oidc`. **Never defaulted** — an empty audience accepts every token the issuer ever minted, including other applications' |
| `OIDC_TENANT_CLAIM` | `tenant_id` | Dotted paths walk nested claims (`realm_access.tenant`) |
| `OIDC_SCOPE_CLAIM` | `scope` | Accepts the space-delimited string or a JSON array |
| `OIDC_LEEWAY` | `30s` | Clock skew tolerance on `exp`/`nbf` — not a grace period |
| `OIDC_MAX_KEY_AGE` | `15m` | How long a cached JWKS is served before revalidation. This is what makes key **revocation** take effect |
| `POSTGRES_MAX_OPEN_CONNS` | `10` | Explicit on purpose: pool defaults read the *node's* CPU count, not the cgroup limit, so 20 replicas exhaust `max_connections` |
| `GRPC_MAX_CONCURRENT_STREAMS` | `250` | grpc-go's default is effectively unbounded — this is a DoS control |
| `GRPC_MAX_CONNECTION_AGE` | `30m` | Forces periodic GOAWAY so rolling deploys actually rebalance |
| `SHUTDOWN_DRAIN_DELAY` | `5s` | Health flips to NOT_SERVING, then waits. Kubernetes deregisters asynchronously, so a pod that stops instantly still gets traffic |

Licensed under [Apache-2.0](LICENSE). Secrets use a [`Secret` type](internal/platform/config/config.go) that redacts through
`String`, `MarshalJSON`, **and** `LogValue` — the three routes by which a password normally
escapes. Covering only one is the usual mistake.

---

## Toolchain notes

**No `protoc`.** buf compiles `.proto` files with a pure-Go compiler. `task gen`
works on a machine that has only Go.

**No `make`.** Windows does not ship it. [`Taskfile.yml`](Taskfile.yml) commands may invoke
only `go`, `docker`, `kubectl`, and `git`, because Task's embedded shell has no `rm`, `cp`,
`sed`, `awk`, or `jq` builtins on Windows. Anything filesystem- or text-shaped lives in
[`cmd/devtool`](cmd/devtool/main.go) in Go.

**`go test -race` will not run on stock Windows.** The race detector links the C++
ThreadSanitizer runtime, so it requires cgo and therefore a C compiler. This is a permanent
limitation, not a configuration gap. `task test:race` detects it and prints the two
real options (a container, or WinLibs + `CGO_ENABLED=1`) instead of a bare
`-race requires cgo`. CI runs `-race` on Linux.

**`.gitattributes` is the first commit in this repo's history**, before any generated file
existed. With `core.autocrlf=true` — a common Windows default — git checks out CRLF while
every generator writes LF, which silently breaks the codegen diff test, golden files, and
`gofmt` in CI, for a reason a newcomer cannot diagnose.

**Docker Desktop + testcontainers:** testcontainers resolves the daemon via
`~/.testcontainers.properties`, `DOCKER_HOST`, then the *default* named pipe — it does not
read Docker contexts. On Docker Desktop the active context is usually `desktop-linux`, so a
healthy daemon can be invisible. `task doctor` detects and explains this.

---

## Making it yours

The module path is the placeholder `github.com/example/gomicro`.

`cmd/rename` (M11) will rewrite `go.mod`, every import, `buf.gen.yaml`'s
`go_package_prefix`, and the image references in one command, then delete itself. Until it
lands, change `go.mod` and the `go_package_prefix` in [`buf.gen.yaml`](buf.gen.yaml), then
run `task gen`.

Because managed mode owns `go_package`, **no `.proto` file hard-codes an import path** —
forking is one line in `buf.gen.yaml`, not one line per proto.

### Adding an RPC

1. Add it to [`proto/order/v1/order.proto`](proto/order/v1/order.proto) with a
   `google.api.http` binding and protovalidate rules.
2. `task gen`
3. Implement the business rule in `internal/order` and add a case to the store contract if
   it touches persistence.
4. Add the thin adapter method in [`internal/grpcapi/server.go`](internal/grpcapi/server.go).
5. Add a `Rule` for it in [`internal/grpcapi/policy.go`](internal/grpcapi/policy.go).

**Step 5 is not optional, and skipping it is not a lint warning.** `NewServer` calls
`Policy.ValidateCoverage` after registration, so a server with an uncovered RPC returns an
error instead of a server — the process does not start, `app.New` fails, and every test using
the harness fails with a message naming the method and the file to edit. (Not a *compile*
error: the check is at construction, because the method set is only knowable once the server
is built.)

---

## Roadmap

**Done:** M0–M1 foundation · M2 toolchain guards · M3 interceptors + observability ·
M5 auth · plus an evidence-backed cuts pass.

**Remaining, in order:**

| | Milestone | What it delivers | Scope change |
|---|---|---|---|
| **next** | **M4** Postgres | GORM + goose + testcontainers, the same store contract run against a real database, N+1 query-count guard, schema-drift check | — |
| | **M6** REST edge | grpc-gateway in **client mode**, so the HTTP path runs the same interceptors | — |
| | **M7** Rate limiting | Redis GCRA per tenant+method, tested against miniredis | **reduced** — cache-aside cut |
| | **M8a** Outbox | Table + relay (`FOR UPDATE SKIP LOCKED`) behind the existing `EventPublisher` port | **split** |
| | **M10** Deploy | Dockerfile, compose, kustomize, e2e tier — and the first real boot of the Keycloak realm | — |
| | **M8b** JetStream | Publisher + consumer + DLQ, against the embedded nats-server | **split**, moved after M10 |
| | **M9** Client | `internal/platform/client`: service config, deadline budget, trace/principal propagation | **reduced** — second service cut |
| | **M11** Forkability | `cmd/rename`, ADRs, `DELETING.md` | **reduced** — `cmd/scaffold` cut |

Every "reduced" and "split" above came out of a review that asked what this template
over-engineers. The reasoning is recorded in the commit history rather than restated here —
the short version is that a second service proved the *client*, which is the reusable part, so
only the client survived.

### Why M5 came before M4

Authentication changes the repo's public posture. Every milestone after it would otherwise
have been built and tested on a service that authenticated nobody, and each one would have
needed revisiting once auth landed. It also let the `SECURITY.md` warning be **deleted**
rather than maintained.

### Known gaps

- **The Keycloak realm has never been imported by a running Keycloak** (Docker was
  unavailable). Its structure is tested; its acceptance by Keycloak is not. M10.
- **Streams have no admission control.** They are bounded only by
  `grpc.MaxConcurrentStreams`; a long-lived watch holding a concurrency slot sized for the
  database pool would be worse than not limiting it. A fork adding streaming work that
  touches the database must revisit this.
- **`-race` does not run locally on Windows** (no cgo). It runs on Linux in CI.

Each milestone lands on its own branch and ends with a green `task verify`.
