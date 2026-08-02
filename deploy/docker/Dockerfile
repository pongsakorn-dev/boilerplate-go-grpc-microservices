# Multi-stage, multi-TARGET. One file builds all three binaries.
#
#   docker build -f deploy/docker/Dockerfile --target orderd  -t orderd  .
#   docker build -f deploy/docker/Dockerfile --target migrate -t migrate .
#   docker build -f deploy/docker/Dockerfile --target worker  -t worker  .
#
# Targets rather than three Dockerfiles because the build stage is identical for all three,
# and three copies of it drift: someone bumps the Go version in one and not the others, and
# the difference surfaces as a binary that behaves subtly differently in production.

# ---------------------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# go.mod and go.sum FIRST, as their own layer.
#
# Dependencies change far less often than source, so this layer is reused across almost every
# build. Copying everything at once instead re-downloads the whole module graph on every
# source edit -- which on this project is 240+ modules.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# CGO_ENABLED=0 produces a STATIC binary, which is what makes distroless/static viable: there
# is no libc in the final image to link against. It also removes an entire class of
# glibc-vs-musl surprises between the build image and the runtime one.
#
# -trimpath strips local filesystem paths from the binary, so a panic in production does not
# print the build machine's directory layout -- and builds become reproducible regardless of
# where the checkout lives.
#
# -ldflags "-s -w" drops the symbol table and DWARF. Smaller image, and a stack trace still
# has function names because Go keeps those separately.
ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=linux

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/orderd  ./cmd/orderd  && \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/migrate ./cmd/migrate && \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/worker  ./cmd/worker

# ---------------------------------------------------------------------------------------
# Runtime targets
# ---------------------------------------------------------------------------------------
#
# distroless/static:nonroot -- no shell, no package manager, no libc, no busybox.
#
# That is a security control rather than minimalism for its own sake: an attacker with
# arbitrary file write and no way to execute a shell is dramatically less dangerous than one
# with /bin/sh. It is also why every ENTRYPOINT below is EXEC form. Shell form
# (`ENTRYPOINT cmd`) wraps the process in `/bin/sh -c`, which does not exist here -- and even
# where it does, it makes the shell PID 1, so SIGTERM goes to the shell and the Go process
# never gets it. The pod then sits out its whole terminationGracePeriodSeconds and is
# SIGKILLed mid-request, on every single deploy.
#
# :nonroot runs as uid 65532 with no shell and no home. The kustomize base also sets
# runAsNonRoot, so the guarantee survives someone rebuilding on a different base image.

FROM gcr.io/distroless/static:nonroot AS orderd
COPY --from=build /out/orderd /orderd
EXPOSE 50051 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/orderd"]

FROM gcr.io/distroless/static:nonroot AS migrate
COPY --from=build /out/migrate /migrate
USER nonroot:nonroot
# No default command: `migrate` with no argument prints usage and exits non-zero. A migration
# runner that does something by default is a migration runner that eventually does it by
# accident.
ENTRYPOINT ["/migrate"]

FROM gcr.io/distroless/static:nonroot AS worker
COPY --from=build /out/worker /worker
USER nonroot:nonroot
ENTRYPOINT ["/worker"]
