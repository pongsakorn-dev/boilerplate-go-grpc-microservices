# 0003 — The repository does not live in a synchronising folder

**Status:** accepted

## Context

Development began with the repository inside a OneDrive-synchronised directory, which is the
default location for a "Work" folder on a stock Windows install and therefore where a fork will
naturally land.

A file-sync driver and a compiler want opposite things from the same files. The compiler writes
thousands of short-lived objects into the build cache and rewrites files in place; the sync client
watches for changes, opens each file to hash and upload it, and may hold or reparse it while doing
so. The observable results were build failures with `Access is denied` on files that had just
been written, and `.git` objects in an inconsistent state after an interrupted sync.

Neither failure names its cause. Both look like a broken toolchain.

## Decision

The repository lives at `C:\Work\boilerplate-go-grpc-microservices` — a plain local directory
outside any synchronised tree. Defender exclusions are added for the repository and for the Go
build cache.

## Consequences

**Good.** Builds are reproducible and noticeably faster, and `git` stops producing errors that
have nothing to do with git.

**Bad.** The working tree is no longer backed up by the sync client. That is the correct trade —
the backup for source code is the remote, not a file syncer that cannot tell a commit from a
temporary object — but it is a real change in behaviour and worth stating rather than discovering.

## Applies to your fork

Any sync client with the same behaviour: OneDrive, Dropbox, Google Drive, iCloud Drive. If a build
fails with a permission error on a file nothing should be holding, this is the first thing to
check. It is not a Windows-only problem; it is only most common there because the default folders
are synchronised.
