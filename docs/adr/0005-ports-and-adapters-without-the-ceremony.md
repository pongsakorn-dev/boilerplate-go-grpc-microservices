# 0005 — Ports and adapters, without the ceremony

**Status:** accepted

## Context

The first question anyone asks about this repository's shape is some version of *"why this
instead of hexagonal architecture, or clean architecture, or a layered design?"*

The honest answer is that **it is hexagonal**. `internal/order` declares the ports —
`Store`, `Atomic`, `EventPublisher` — and `ordermem`, `orderpg` and `orderproj` are adapters
implementing them. Dependencies point inward and the domain points at nothing. That is ports
and adapters as Cockburn described it.

What is missing is the vocabulary and roughly four conventions that usually travel with it.
Nothing in the tree says so, which is why the question keeps getting asked, and why a reader
comparing this against a textbook diagram concludes something is absent when it is present
under a different name.

Two facts frame every choice below.

**The rule is compiler-checked, not documented.** [`test/layout_test.go`](../../test/layout_test.go)
walks the real transitive import graph with `go/packages` and fails the build if the domain
reaches gRPC, GORM, protobuf, a telemetry SDK or `database/sql`. Most codebases that claim
this claim it in a README and leak a `*gorm.DB` into a service method within the year.

**Go's package model is not Java's.** Several conventions that are free in a language with
class-level visibility cost real API surface in Go, because the unit of encapsulation is the
package. That is the reason for three of the four omissions, and it is why copying a diagram
drawn for another language produces worse Go.

## Decision

Keep the dependency inversion. Drop the ceremony. Specifically:

**No `domain/` + `application/` + `infrastructure/` split.** Clean Architecture separates
Entities from Use Cases. Here [`service.go`](../../internal/order/service.go) sits beside
[`order.go`](../../internal/order/order.go) in one package. Splitting them would force every
type and helper the use cases touch to be **exported**, widening the public API of the domain
purely to satisfy a directory layout — the opposite of what the split is for.

**No inbound ports.** Hexagonal symmetry gives the driving side ports too: an `OrderUseCase`
interface for `internal/grpcapi` to depend on. Instead
[`grpcapi.OrderServer`](../../internal/grpcapi/server.go) holds a concrete `*order.Service`.
An interface with one implementation and one consumer is indirection that buys nothing, and
Go's convention is to define an interface where it is *consumed* — which the outbound ports
already do, and an inbound port would not.

**Sliced by domain, not by layer.** `internal/order/` holds the domain and its adapters as
subpackages. The usual arrangement puts every adapter under one `infrastructure/`, so adding
one feature touches four distant directories.

**`internal/platform/` is not "infrastructure".** It is cross-cutting mechanism — config,
auth, interceptors, outbox, observability — that the domain never sees.
`TestPlatformDoesNotImportServices` keeps it free of order-specific code so it stays
extractable into a shared module later.

## Consequences

**Good.** The one boundary that is expensive to retrofit is the one that is enforced. Once
`*pb.Order` reaches the SQL layer, removing it is a simultaneous rewrite of every layer;
keeping it out from day one costs a mapping file.

**Good.** The default test tier runs with the Docker daemon stopped *because* of this
boundary, not incidentally. Business logic is testable against a hand-written fake that a
shared contract suite proves equivalent to the real store.

**Good.** Fewer packages, and every one of them has a reason a reader can state.

**Bad.** [`convert.go`](../../internal/grpcapi/convert.go) is hand-written mapping code, and
it is dull to maintain. That is the actual price, paid on every proto change.
`TestAllProtoFieldsAreAcknowledged` stops a new field being silently unmapped.

**Bad.** `Atomic.InTx(ctx, func(Store, EventPublisher) error) error` is an awkward signature.
It is the cost of keeping transactions out of the domain while still forcing the outbox write
into the business transaction. The alternative — handing the callback a `*gorm.DB` — breaks
three things at once: the domain imports the driver, the in-memory fake can no longer
implement `Atomic`, and every transactional business test silently becomes a Docker test.

**Bad.** Readers who know the textbook version look for the missing layers and assume an
oversight. This document is the mitigation.

## The escape seam

A fork that wants the full tri-layer can have it without touching the adapters: move
`service.go` into `internal/order/app/`, export what it needs, and leave the ports and their
implementations exactly where they are. Nothing in `internal/platform/` or `internal/grpcapi/`
depends on the domain's internal file layout — only on the exported `Service` and the port
interfaces.

Going the other way is the expensive direction, which is why the boundary exists now rather
than later.

## Why this is an ADR and not a comment

By [ADR 0001](0001-decisions-live-next-to-the-code.md) reasoning belongs beside the code it
decided, and this repository keeps to that: the layering *rule* lives in
[`test/layout_test.go`](../../test/layout_test.go), where the failure message explains it to
whoever broke it.

But the rule is not the decision. What that test encodes is "the domain imports nothing
heavy". What it cannot encode is the choice among alternatives — why no tri-layer, why no
inbound ports, why slice by domain — because that choice is about the shape of the whole
repository and no single file owns it. Attaching it to any one file would put a whole-repo
argument in an arbitrary place.

That is exactly the case ADR 0001 reserves for a written ADR: a decision with no code to sit
next to.
