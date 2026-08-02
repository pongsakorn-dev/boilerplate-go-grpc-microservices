# 0002 — What was cut, and the test every cut had to pass

**Status:** accepted

## Context

The original plan was larger than what shipped. A review partway through asked one question of
every component — *what does this teach that its absence would not?* — and several did not
survive it.

Those removals need writing down somewhere, because a template is judged as much by what it does
not contain as by what it does, and "we ran out of time" and "we decided against it" look
identical from outside. This is the only decision in the set with no code to sit next to: the code
is what was deleted.

## The test a cut had to pass

Something was removed when **both** were true:

1. **Its absence is honest.** A reader can tell it is missing, and the README says so. A silent
   gap is a lie; a listed gap is a scope decision.
2. **Adding it later is additive, not a rewrite.** Nothing that shipped had to be undone to add
   it back.

Anything failing (2) stayed, however large — which is why keyset pagination, the outbox and the
tenant column are all here. Retrofitting any of them is a breaking API change or a migration.

## What was cut

**Idempotency keys.** Planned as a table joining the business transaction. Cut, and it is the cut
with the sharpest consequence: without it, no mutation is safe to retry, so
`internal/platform/client` ships with retries opt-in per method and no mutation opted in. That
constraint is written into the retry policy and tested. Adding idempotency later adds a table and
an interceptor; nothing already written has to change.

**Cache-aside over Redis.** A cache's correctness is entirely a property of the invalidation rules
of the data being cached, so a generic one teaches a pattern that is wrong for most domains.
Redis stayed for rate limiting, where the semantics are the same everywhere.

**A second service.** Planned as `inventoryd`, to make the inter-service hop real. Cut once it
was clear that the *client* is the reusable half and a second copy of a CRUD domain is not. The
client shipped and is tested against the real server over bufconn — and the README says plainly
that nothing calls it.

**`cmd/scaffold`.** A generator for service N+1. Cut with the second service: a generator whose
output nobody has ever run is a liability, and `cmd/rename` — which *is* exercised, end to end,
by a test that renames the whole repository and then builds and tests it — covers the case a fork
actually hits.

**A per-replica token-bucket rate limiter.** 100rps × replicas, reset on every deploy and every
autoscaling event, is a number that means nothing. Replaced by a local admission limiter sized
from the database pool (which protects the real bottleneck) plus Redis GCRA for actual quotas.

**A request-id interceptor.** The OTel `trace_id` already is the correlation id. Deleting it
removed an interceptor, its tests, and a piece of attacker-controlled metadata.

**`internal/platform/clock`.** `testing/synctest` is GA in the toolchain this targets, so the
`Clock` interface every service used to grow is unnecessary.

**Load and performance gates.** Hardware-dependent thresholds flap, flapping gates get disabled,
and a disabled gate is worse than no gate because it looks like coverage. Go benchmarks remain.

**Connect-RPC, mock generation, testify, Helm, GoReleaser, AIP-160 filters.** Each is defensible;
each would have cost more in this template than it taught. The one-line reasons are in the
README's Key decisions table.

## Consequences

**Good.** Roughly eighteen internal packages instead of forty, and every one has a named test. The
README's status table lists what is absent, so the absences are legible.

**Bad.** Three of these — idempotency, the second service, the cache — are things a real service
eventually needs, and a reader who assumes "this template has it because it has everything else"
will be wrong. That is what the README's status table and *Known gaps* section exist to prevent,
and it is why they are guarded by tests rather than maintained by good intentions.
