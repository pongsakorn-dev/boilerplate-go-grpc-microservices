# 0004 — Generated protobuf code is regenerated, never rewritten

**Status:** accepted

## Context

Forking this template means changing the Go module path, which appears in about ninety-five
files. The obvious implementation is a text substitution across the tree, and `cmd/rename` is
mostly exactly that.

It does not work for `gen/`, and the way it fails is the reason this decision is written down.

A generated `.pb.go` embeds its `FileDescriptorProto` as a raw byte string — the serialized
descriptor the protobuf runtime parses at init to register the file. Protobuf wire format is
**length-prefixed**. The descriptor contains `go_package`, which contains the module path.

Substituting a module path of a different length leaves every length prefix after that point
addressing the wrong bytes. Measured: `github.com/example/gomicro` is 26 characters,
`github.com/acme/orders` is 22, and a real descriptor goes from 2700 bytes to 2696 while every
prefix still claims the old sizes.

What makes it dangerous is where it surfaces:

```
panic: runtime error: slice bounds out of range [-4:]
  google.golang.org/protobuf/internal/filedesc.(*File).unmarshalSeed
  gen/go/order/v1/order.pb.go:997  file_order_v1_order_proto_init()
```

The file still compiles. `go vet` still passes. The failure arrives inside `init()`, before
`main`, in a stack trace that mentions neither the rename nor the module path. A rename tool
verified with `go build` ships this.

## Decision

`cmd/rename` skips `gen/` entirely and regenerates it with `buf` after rewriting everything else.

If regeneration cannot run — no module cache, no network — the tool **stops and reports it**,
leaving `gen/` still importing the old module path so the tree fails to compile. A loud failure
that names the fix is strictly better than a silent one that panics in production.

## Consequences

**Good.** The result is correct by construction rather than by careful string handling, and it
stays correct if the generator changes what it embeds.

**Bad.** Renaming now requires a working codegen toolchain, where a pure text substitution would
have needed nothing. That is the cost of the guarantee, and `-skip-codegen` exists for anyone who
wants to regenerate separately — it prints the exact command and leaves a tree that does not
build until they run it.

**Bad.** Renaming takes about forty seconds longer.

## Why this is an ADR and not a comment

By [ADR 0001](0001-decisions-live-next-to-the-code.md), reasoning belongs next to the code it
decided — and this reasoning did live in `cmd/rename/main.go`. But that file **deletes itself** on
a successful rename, so in every fork the code is gone and the argument would go with it.

A fork that later writes its own codegen tooling needs this finding more than the template did.
It has no code to sit next to, which is exactly the case ADR 0001 reserves for a written ADR.

## How it was tested, in the template

Two ways, cheap and expensive. Both lived in the rename tool's own package, so **in your fork
they are gone** — which is correct, since they test a tool that no longer exists, and is the
reason this document does exist.

The cheap one took the real descriptor from the real generated package, applied the naive
substitution to its bytes, and asserted the protobuf runtime refused to parse the result. It ran
in milliseconds on every test run, so the justification for this design was re-derived constantly
rather than remembered.

The expensive one renamed a copy of the whole repository and then ran **both** `go build ./...`
and `go test ./...` against it. The second command is the one that catches this panic; the first
does not, which is the entire point.

If you build codegen tooling of your own, that pair is the shape to copy: verify the bytes
cheaply on every run, and prove the whole pipeline occasionally.
