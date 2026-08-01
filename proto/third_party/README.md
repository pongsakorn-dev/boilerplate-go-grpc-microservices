# Vendored third-party protobuf definitions

These files are **not part of this project**. They are copied verbatim from upstream so
that `go tool task gen` works offline, with no Buf Schema Registry account and no network
access after the Go module cache is warm.

All three are licensed under the Apache License 2.0. A verbatim copy of that license is in
[`LICENSE`](LICENSE) in this directory, as Apache-2.0 §4(a) requires when redistributing.
Their original copyright and license headers are intact and must stay that way —
`test/thirdparty_test.go` fails the build if any header is removed.

| File | Upstream | Vendored from | License |
|---|---|---|---|
| `buf/validate/validate.proto` | [bufbuild/protovalidate](https://github.com/bufbuild/protovalidate) | `buf export buf.build/bufbuild/protovalidate` (buf v1.72.0, 2026-07-31) | Apache-2.0 |
| `google/api/annotations.proto` | [googleapis/googleapis](https://github.com/googleapis/googleapis) | `grpc-ecosystem/grpc-gateway@v1.16.0/third_party/googleapis/` | Apache-2.0 |
| `google/api/http.proto` | [googleapis/googleapis](https://github.com/googleapis/googleapis) | `grpc-ecosystem/grpc-gateway@v1.16.0/third_party/googleapis/` | Apache-2.0 |

**None of these files has been modified.** Apache-2.0 §4(b) requires prominent notices on
modified files; recording "unmodified" here makes that a stated fact rather than an
assumption, for anyone auditing the tree.

## Why vendored rather than declared as buf `deps:`

A `deps:` entry in `buf.yaml` resolves from the Buf Schema Registry into buf's own cache
under `%LOCALAPPDATA%` (or `~/.cache`). `go mod download` never warms that cache, so a
declared dependency would make `task gen` require network access and BSR availability on
every cold machine — and would transmit this project's schema to a third party on every
generate. `tools/bufgen_test.go` asserts `buf.yaml` declares no `deps:` and that no
`buf.lock` exists.

## Re-vendoring

```bash
go run github.com/bufbuild/buf/cmd/buf@v1.72.0 export buf.build/bufbuild/protovalidate -o <tmpdir>
```

Copy `<tmpdir>/buf/validate/validate.proto` over the existing file, then update the version
and date in the table above. The `google/api` files change very rarely; they can be taken
from any recent googleapis checkout.
