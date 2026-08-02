# 0001 — Decisions live next to the code

**Status:** accepted

## Context

The plan for this template called for eighteen numbered ADRs, one per locked decision. Writing
them surfaced the problem: for fifteen of the eighteen, the argument was already written — in a
doc comment above the thing it decided, or in a README section, usually in more detail and with
the measurements attached.

An ADR for those would be a *second copy*. Second copies of an argument do not stay equal. The
one next to the code gets updated when the code changes, because it is on screen when you change
it. The one in `docs/adr/` does not, because nothing brings you there.

And the drifted copy is the one that does the damage: a newcomer reads `docs/adr/` precisely
because it looks authoritative and complete.

## Decision

The reasoning for a decision lives with the thing it decided. `docs/adr/README.md` is an index
into those locations, not a restatement of them.

An ADR is written only when a decision has no code to sit next to — a process decision, a
decision about what *not* to build, or one whose subject is the repository itself.

## Consequences

**Good.** One canonical home per argument, which is the same rule the codebase applies to
comments. The argument is on screen when the code is edited. `test/citations_test.go` and
`test/docs_test.go` already fail the build when a reference goes stale, so the index cannot rot
quietly.

**Bad.** There is no single file to read to absorb every decision. The index mitigates it; it does
not remove it. A reader who wants the full picture reads the README and then follows links,
which is more work than reading one directory.

**Bad.** "Where does this belong?" becomes a judgement call on every new decision, where a
numbered sequence is mechanical. The test in `docs/adr/README.md` is: could someone changing this
code plausibly encounter the argument? If yes, it belongs in the comment.

## Alternatives considered

**Eighteen ADRs as planned.** Rejected: fifteen would have been summaries of existing doc
comments, and the repository's own stated principle is that a claim should have exactly one place
it is made.

**ADRs only, comments stripped to what.** Rejected for the opposite reason. The doc comments here
carry measurements — lock timings, deduplication windows, compile times — that belong where the
code that depends on them is read.
