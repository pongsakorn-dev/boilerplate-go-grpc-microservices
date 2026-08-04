# Deleting what you do not need

Most of this template is optional. This is the list, **in an order that works**: follow it top to
bottom and no step ever breaks a later one, because nothing above depends on anything below.

Two things before you start.

**Most subsystems can be turned off without deleting anything.** Each section names the
configuration that disables it. Do that first, run your tests, and confirm you really do not want
it — deleting is a lot more work than setting an environment variable to empty, and much harder to
undo.

**Delete the tests too.** Every section lists them. A subsystem's tests are what made it
trustworthy; leaving them behind after removing the code just breaks the build, and deleting the
code while keeping a *passing* test that no longer exercises anything is worse.

After each section: `go build ./... && go test ./...`.

---

## The order, and why it is this order

| # | Subsystem | Must come before | Because |
|---|---|---|---|
| 1 | Outbound client | — | Nothing in the repo imports it |
| 2 | REST/JSON gateway | — | Only `internal/app` wires it |
| 3 | Redis rate limiting | — | Only the interceptor chain uses it |
| 4 | Order projection | 5, 6 | Imports `internal/platform/events` **and** Postgres |
| 5 | NATS JetStream | 6 | Implements `outbox.Publisher`, so it imports the outbox |
| 6 | Outbox + worker | 7 | The outbox table lives in Postgres |
| 7 | Postgres + GORM | — | Nothing above is left to need it |
| 8 | OIDC | — | Independent, but read the production warning |
| 9 | OpenTelemetry traces | — | Independent |
| 10 | `deploy/` | — | Nothing imports it |

Reversing 4→5→6→7 is the mistake worth naming: delete Postgres first and you are left with an
outbox relay that imports `database/sql` for a table that no longer exists, and the compiler will
not tell you until you have also removed the store.

---

## 1. The outbound client

Calls to other services: deadline budget, opt-in retries, upstream error translation.

**Keep it if** this service will ever call another one. **Nothing currently calls it** — the
second service was cut from the plan — so removing it costs nothing today.

- **Disable:** not applicable; it only runs when you call it.
- **Delete:** `internal/platform/client/`
- **Wiring:** none. No production code imports it.
- **Config:** remove `UpstreamConfig` from `internal/platform/config/config.go`, its three
  `UPSTREAM_*` entries in `Parse`, and the `Upstream` block in `Validate`.
- **Metrics:** remove `Metrics.ClientFor` and the `clients` map from
  `internal/platform/observability/metrics.go`. Keep `latencyBuckets` — the server histogram
  still uses it — but its comment explains a client/server comparison that no longer exists, so
  shorten it rather than leaving a reason for a thing that is gone.
- **Tests deleted with it:** all of `internal/platform/client/*_test.go`.

---

## 2. The REST/JSON gateway

grpc-gateway transcoding HTTP+JSON onto the same gRPC service, in client mode so REST runs the
identical interceptor chain.

**Keep it if** anything speaks to you over HTTP — browsers, webhooks, curl.

- **Disable:** `GATEWAY_ADDR=""`. Supported and tested; the app binds nothing.
- **Delete:** `internal/gateway/`, `gen/openapiv2/`
- **Wiring:** in `internal/app/app.go`, remove `buildGateway` and its call, the `gatewaySrv` and
  `gatewayLis` fields, the `GatewayHandler` accessor, and the gateway steps in `Close`.
- **Protos:** remove the `google.api.http` annotations from `proto/order/v1/order.proto`, the
  `protoc-gen-grpc-gateway` and `protoc-gen-openapiv2` plugins from `buf.gen.yaml`, and the two
  `grpc-gateway` entries from the `tool` directive in `go.mod`. Then `task gen`, and delete
  `gen/go/order/v1/order.pb.gw.go`.
- **Third-party protos:** `proto/third_party/google/api/` is now unused. Removing it also means
  removing its entry from `test/thirdparty_test.go`.
- **Tests deleted with it:** `internal/gateway/*_test.go`.
- **Note:** `internal/testutil/server.go`'s `NewTestGateway` goes too.

---

## 3. Redis rate limiting

Per-tenant, per-method GCRA quotas shared across replicas.

**Keep it if** you have more than one replica and untrusted callers. A per-replica limiter is not
a substitute — see the README.

- **Disable:** `REDIS_ADDR=""`. The app substitutes `ratelimit.AllowAll{}`, a named type rather
  than a nil the interceptor has to check.
- **Delete:** `internal/platform/ratelimit/`, `internal/platform/interceptor/ratelimit.go`
- **Wiring:**
  - `internal/app/app.go`: remove `buildLimiter` and its call.
  - `internal/grpcapi/chain.go`: remove the `Limiter` field from `Deps`, its nil check in
    `NewServer`, and **both** `interceptor.RateLimit(...)` from the unary chain **and**
    `interceptor.RateLimitStream(...)` from the stream chain. Missing the second leaves the
    tree uncompilable after `ratelimit.go` is deleted — this line named only the unary one
    until the streaming quota was added, and the guide was not updated with it.
  - `internal/grpcapi/chainparity_test.go`: add `"RateLimit"` to `declaredStreamGaps`, or
    delete the file. It asserts every interceptor in the unary chain has a streaming
    counterpart unless the gap is written down, so removing one chain's entry and not the
    other's fails it.
- **Config:** remove `RedisConfig`, the `Redis` field, its `REDIS_*`/`RATE_LIMIT_*` entries in
  `Parse`, and the Redis block in `Validate`.
- **Dependencies:** `github.com/redis/go-redis/v9` and `github.com/alicebob/miniredis/v2` leave
  `go.mod` after `go mod tidy`.
- **Tests deleted with it:** `internal/platform/ratelimit/*_test.go`,
  `internal/platform/interceptor/ratelimit_test.go`, and the streaming-quota cases in
  `internal/grpcapi/ratelimit_test.go` (`TestOpeningAStreamSpendsQuotaToo`) and
  `internal/gateway/gateway_test.go` (`TestRetryAfterReachesRestClientsUnderItsRealName`).
- **Compose:** remove the `redis` service from `deploy/compose/docker-compose.yml`.

---

## 4. The order projection

The read model (`order_counts`) the worker maintains from broker events, and the
`processed_events` deduplication that makes it exactly-once.

**Keep it if** you want a worked example of a consumer that is correct under redelivery. It is the
smallest piece here and the one that demonstrates the transaction boundary.

- **Delete:** `internal/order/orderproj/`
- **Wiring:** in `cmd/worker/main.go`, remove the `orderproj` import and the
  `events.NewConsumer(...)` call that takes it — the worker then relays without consuming.
- **Schema:** drop the `order_counts` and `processed_events` tables from
  `internal/platform/migrations/00002_events.sql`. Keep the outbox `failed_at` / `failure_reason`
  columns: those belong to the relay, not the projection.
- **Tests deleted with it:** `internal/order/orderproj/*_test.go`.

---

## 5. NATS JetStream

The publisher, the consumer, the dead-letter handling, and the embedded-server test harness.

**Keep it if** anything outside this service needs to know when an order changes.

- **Disable:** `NATS_URL=""`. The worker falls back to `outbox.LogPublisher`, which prints exactly
  what would be published — so the whole outbox path still runs with no broker installed.
- **Delete:** `internal/platform/events/`
- **Wiring:** in `cmd/worker/main.go`, remove the `events` import and the `if cfg.NATS.URL != ""`
  block, leaving `outbox.LogPublisher` — or your own `outbox.Publisher`.
- **Config:** remove `NATSConfig`, the `NATS` field, its `NATS_*` entries in `Parse`, and
  `validateNATS` plus its call in `Validate`.
- **Dependencies:** `github.com/nats-io/nats.go` and `github.com/nats-io/nats-server/v2` leave
  `go.mod` after `go mod tidy`.
- **Tests deleted with it:** `internal/platform/events/*_test.go` and
  `internal/platform/events/eventstest/`.
- **Compose:** remove the `nats` service and the `nats-data` volume from
  `deploy/compose/docker-compose.yml`.
- **Guards:** `test/tiers_test.go` no longer needs its note about nats-server.

---

## 6. The outbox and the worker

The transactional outbox, the `FOR UPDATE SKIP LOCKED` relay, quarantine, and the process that
runs it.

**Keep it if** you publish events at all. This is the part that makes "the order committed but the
event vanished" impossible; a service that publishes from inside its handler does not have that
property, whatever its README says.

- **Delete:** `internal/platform/outbox/`, `cmd/worker/`, `cmd/prune/`
- **Domain:** this is the invasive one. `internal/order` declares `EventPublisher` and
  `Store.InTx` hands one to the callback, so removing the outbox means:
  - `internal/order/store.go`: remove the `EventPublisher` interface and change `Atomic.InTx` to
    take only a `Store`.
  - `internal/order/service.go`: remove the `pub.Publish(...)` calls in `Create` and `Cancel`.
  - `internal/order/ordermem/`, `internal/order/orderpg/`: remove the `Publish` and `Events`
    methods and the `outboxRow` model.
  - `internal/order/ordertest/contract.go`: remove the two transactional subtests — they exist to
    assert the outbox invariant and assert nothing without it.
- **Schema:** drop the `outbox` table and its index from
  `internal/platform/migrations/00001_orders.sql`, delete `00003_retention.sql`,
  `00004_outbox_observability.sql` and `00005_outbox_trace.sql`, and delete `00002_events.sql`
  entirely if you also did step 4.
- **Config:** remove `OutboxConfig`, the `Outbox` field, and its `OUTBOX_*` entries. Remove
  `RetentionConfig`, the `Retention` field, the `RETENTION_*` entries, and the retention block
  in `Validate` — including the check that `RETENTION_PROCESSED_EVENTS` exceeds
  `NATS_STREAM_MAX_AGE`, which has nothing left to protect once neither table exists.
- **Deploy:** remove `deploy/k8s/base/worker.yaml` and `prune-cronjob.yaml` and their lines in
  `deploy/k8s/base/kustomization.yaml`, the `worker` service in compose, and the `worker` and
  `prune` targets from `deploy/docker/Dockerfile` and `Taskfile.yml`'s `docker:build`. Also
  remove the `db:prune` and `db:prune:dry-run` targets.

### Two halfway houses

Both keep the outbox and drop one thing built on it. Neither is covered by
`task verify:profile`: that test walks the numbered sections' **Delete** bullets, and these are
prose. So they are the instructions here most likely to rot, and the lists below were verified
by executing them once by hand.

> [!WARNING]
> The first drafts of both paragraphs left the repository **unable to compile**, because neither
> named the test files referencing the deleted code. `go test ./...` failed on
> `TestTaskTargetsReferenceRealPaths` before any build tag was involved. If you edit these, run
> `go build ./... && go test ./...` **and** `task lint` afterwards — the latter compiles the
> tagged tiers, which is where the leftovers hide.

**Keeping the outbox but not the pruning.** Delete `cmd/prune/`,
`internal/platform/outbox/prune.go`, `internal/platform/migrations/00003_retention.sql`,
`deploy/k8s/base/prune-cronjob.yaml` and its line in `kustomization.yaml`, the `prune` target in
`deploy/docker/Dockerfile`, and the `prune` line in `Taskfile.yml`'s `docker:build`. Then:

- **Taskfile:** also remove the `db:prune` and `db:prune:dry-run` targets.
  `test/taskpaths_test.go` runs in the DEFAULT tier and fails when a target names a path that no
  longer exists.
- **Config:** remove `RetentionConfig`, the `Retention` field, the `RETENTION_*` entries in
  `Parse`, and the retention block in `Validate` — including the check that
  `RETENTION_PROCESSED_EVENTS` exceeds `NATS_STREAM_MAX_AGE`, which then has nothing left to
  protect. Its cases in `internal/platform/config/config_test.go` go too.
- **Tests:** delete `internal/platform/outbox/prune_integration_test.go` — **but move
  `outboxState`, `insertOutbox`, `explain` and `containsAny` out of it first.**
  `observer_integration_test.go` is their only other user and will not compile without them.
  This is the step both earlier drafts of this paragraph missed.

You then own the fact that both tables grow without bound — fine for a service publishing a few
thousand events a day, not fine for one publishing a few thousand a second.

**Keeping the outbox but not its metrics.** Delete `internal/platform/outbox/observer.go`,
`internal/platform/migrations/00004_outbox_observability.sql` and
`internal/platform/outbox/observer_integration_test.go`. Then:

- **Wiring:** in `cmd/worker/main.go` remove the observer goroutine, the registry and the admin
  server — returning that binary to opening no listener at all — and the imports that leaves
  unused.
- **Config:** remove `OutboxConfig.ObserveInterval` and its `OUTBOX_OBSERVE_INTERVAL` entry.
- **Deploy:** drop the `admin` port, `ADMIN_ADDR` and `POD_IP` from
  `deploy/k8s/base/worker.yaml`, and the `9091:9090` mapping plus `ADMIN_ADDR` and
  `OUTBOX_OBSERVE_INTERVAL` from compose. Remove `9091` from `requireFreePorts` in
  `test/e2e/compose_test.go`, and delete `TestTheWorkerExposesOutboxHealth` and
  `TestQuarantinedRowsBecomeVisible` from `test/e2e/events_test.go`.
- **Metrics:** `observability.NewProcessRegistry` loses its only caller. Keep it or delete it,
  but do not leave its doc comment claiming `cmd/worker` uses it.

Understand what you are giving up: a quarantined row and a wedged relay both become invisible
again, and the worker has no probes precisely because those metrics were the alternative.

---

## 7. Postgres and GORM

The persistent store, the tenant guard, migrations, and the integration-test harness.

**Keep it if** your data outlives the process — which is to say, keep it.

- **Disable:** `STORE_DRIVER=memory`, which is the default. The in-memory store satisfies the same
  contract suite, so it is a real implementation and not a stub.
- **Delete:** `internal/order/orderpg/`, `internal/platform/gormx/`,
  `internal/platform/migrations/`, `internal/platform/testdb/`, `cmd/migrate/`
- **Wiring:** in `internal/app/app.go`, remove the `postgres` arm of `buildStore` and its
  `gormx.Open` call, leaving only `ordermem`.
- **Config:** remove `PostgresConfig`, the `Postgres` field, its `POSTGRES_*` entries, and the
  `STORE_DRIVER` validation that requires a DSN. Note `Server.AdmissionLimit` derives from
  `Postgres.MaxOpenConns` — give it its own default.
- **Dependencies:** `gorm.io/*`, `github.com/jackc/pgx/v5`, `github.com/pressly/goose/v3` and
  `github.com/testcontainers/*` leave `go.mod` after `go mod tidy`.
- **Tests deleted with it:** every test file behind the `integration` build tag, and the Postgres arm of
  `internal/order/ordertest/contract.go`.
- **Compose:** remove `postgres`, `postgres-data` and the `migrate` service.
- **Guards:** `test/tiers_test.go` no longer needs its testcontainers entry.

---

## 8. OIDC authentication

JWT verification, the JWKS cache, and the hostile-issuer test suite.

**Keep it.** The alternative is `AUTH_MODE=dev`, which authenticates nobody.

- **Disable:** `AUTH_MODE=dev`. `config.Validate` **refuses this when `APP_ENV=production`**, and
  that refusal is the single most important line in the config package. If you delete OIDC you
  must also delete that check, at which point nothing stops an unauthenticated service reaching
  production. Do not do this because auth is inconvenient during development — dev mode already
  covers that, out of the box, with a warning on every startup.
- **Delete:** `internal/platform/auth/oidc.go`, `internal/platform/auth/jwks.go`,
  `internal/platform/auth/testjwks/`, `deploy/keycloak/`, `test/e2e/oidc/`,
  `deploy/compose/docker-compose.oidc.yml`, and the `keycloak` service in compose.
- **Wiring:** in `internal/platform/auth/verifier.go`, remove the `oidc` arm of `NewVerifier`
  and the `AllowInsecureIssuer` startup warning above it. Keep the erroring default arm: an
  unknown `AUTH_MODE` must fail, never fall back.
- **Config:** remove `OIDCConfig`, its `OIDC_*` entries, and the two `Validate` blocks that
  refer to them — the `AUTH_MODE=oidc` completeness check and the
  `OIDC_ALLOW_INSECURE_ISSUER` production refusal.
- **Taskfile:** `verify:e2e` passes `-p 1` only because two e2e packages publish the same host
  ports. With `test/e2e/oidc/` gone there is one package left and the flag is no longer needed,
  though leaving it costs nothing.
- **Dependencies:** `github.com/golang-jwt/jwt/v5` leaves `go.mod`.
- **Compose:** remove the `keycloak` service and the `auth` profile.

---

## 9. OpenTelemetry traces

Distributed tracing, trace-correlated logs, and the OTLP exporter.

**Keep it if** you have more than one service, or more than one replica and a latency question.

- **Disable:** `OTEL_EXPORTER_OTLP_ENDPOINT=""`, the default. Everything is instrumented and
  nothing is exported, which is why a fresh clone needs no collector.
- **Delete:** `internal/platform/observability/tracing.go`, and `NewTraceHandler` from
  `internal/platform/observability/logging.go`
- **Wiring:**
  - `internal/app/app.go`: remove the `NewTracerProvider` call and the `flushTraces` step.
  - `internal/grpcapi/chain.go`: remove the `otelgrpc` stats handlers.
  - `internal/platform/client/client.go`: remove its `otelgrpc` stats handler.
- **Config:** remove `TelemetryConfig` and its `OTEL_*` entries.
- **Dependencies:** all of `go.opentelemetry.io/*` leave `go.mod`.
- **Keep Prometheus.** It is a separate subsystem in the same package and answers a different
  question: metrics tell you something is wrong, traces tell you where.

---

## 10. Deployment

The Dockerfile, compose stack and Kubernetes manifests.

**Keep it if** you deploy anywhere. Delete it if your organisation generates manifests centrally
and a second, drifting copy is worse than none.

- **Delete:** `deploy/`
- **Wiring:** remove the `verify:deploy`, `docker:build`, `up` and `down` targets from
  `Taskfile.yml`.
- **Tests deleted with it:** `deploy/k8s/manifest_test.go`, which is a default-tier test — so
  `go test ./...` gets slightly faster and stops asserting anything about how you ship.

---

## What cannot be removed

- **`internal/platform/apperr`** — every interceptor and both transports map errors through it.
- **`internal/platform/config`** — the only package that reads the environment, by rule and by
  test.
- **`internal/platform/interceptor`** recovery, logging and errmap — a server without recovery
  turns one panic into a dead process, and without errmap every domain error reaches callers as
  `codes.Unknown`.
- **The authorisation policy** — `NewServer` refuses to boot when a served method has no policy
  entry. That is deliberate: a new RPC cannot ship without a conscious auth decision.
- **`test/`** — the guards are what keep the rest of this honest. Deleting them removes the only
  thing standing between the documentation and fiction.
