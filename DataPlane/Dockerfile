# syntax=docker/dockerfile:1.7
###############################################################################
# Powerhouse Cache — production container image
#
#   • Multi-stage      → build tools never ship in the final image.
#   • Multi-arch       → one Dockerfile builds linux/amd64 AND linux/arm64
#                        (x86 t3.micro OR Graviton t4g.small — see DEPLOYMENT.md).
#   • Distroless+static→ no shell, no libc, no package manager: tiny CVE surface.
#   • Non-root         → runs as uid/gid 65532; never root.
#   • Reproducible     → -trimpath, stripped, pinned base, no timestamps.
#   • Self-reporting   → a baked-in healthcheck binary (distroless has no shell).
#
# IMPORTANT: the build context is the REPOSITORY ROOT, not DataPlane/, because
# the build needs the workspace file (go.work) that lives one level up:
#
#   docker build -f DataPlane/Dockerfile -t powerhouse-cache .
#   # or via compose:  docker compose build
###############################################################################

# ---- Stage 1: build ---------------------------------------------------------
# Pin to a digest in production; Dependabot/Renovate keeps it fresh, e.g.:
#   FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:<digest> AS build
# We build ON the native builder platform ($BUILDPLATFORM) and cross-compile to
# the requested target ($TARGETOS/$TARGETARCH) — far faster than QEMU-emulating
# the whole toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# Provided automatically by buildx for cross-compilation.
ARG TARGETOS
ARG TARGETARCH
# Provenance metadata, injected by CI (git SHA / tag / build timestamp).
ARG VERSION=dev
ARG GIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

# Supply-chain hardening of the build itself:
#   -mod=readonly → the build can NEVER silently add/upgrade a dependency. If an
#     import isn't already recorded in go.mod/go.sum, the build FAILS instead of
#     reaching out to the network and mutating the module graph. So whatever ends
#     up in the image is exactly what was reviewed and checksummed in git.
ENV GOFLAGS=-mod=readonly

# 1) Dependency layer FIRST. Copy only the module graph so this expensive layer
#    is cached until go.mod/go.sum actually change. go.sum may not exist yet
#    (the project has zero third-party deps today) — the wildcard keeps COPY
#    from failing, and future deps will be picked up automatically.
COPY go.work ./
COPY DataPlane/go.mod DataPlane/go.sum* DataPlane/
# go mod download fetches deps; go mod verify checks each downloaded module's
# contents against its go.sum checksum. Both are no-ops today (no deps) and
# become real integrity gates the moment a dependency is added.
RUN --mount=type=cache,target=/go/pkg/mod \
  cd DataPlane && go mod download && go mod verify

# 2) Source layer.
COPY DataPlane/ DataPlane/

# 3) Static, stripped, reproducible cross-compile of BOTH binaries:
#      • powerhoused  — the cache server
#      • healthcheck  — the in-image liveness probe (no shell needed at runtime)
#    CGO_ENABLED=0 → a fully static binary that needs no libc on the base image.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  cd DataPlane && \
  GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.gitSHA=${GIT_SHA} -X main.buildDate=${BUILD_DATE}" \
  -o /out/powerhoused ./cmd/powerhoused && \
  GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
  -ldflags="-s -w" \
  -o /out/healthcheck ./cmd/healthcheck

# Pre-create an EMPTY WAL directory. Because distroless has no shell to `chown`
# at runtime, we copy it into the final image with the non-root uid/gid so that
# a freshly-created Docker named volume inherits the correct ownership and the
# non-root process can actually write the WAL. (See the durability note below.)
RUN mkdir -p /out/data

# ---- Stage 2: runtime (distroless static, non-root) -------------------------
# distroless "static" = just CA certs + tzdata + /etc/passwd; no shell, no apt,
# no libc. "nonroot" variant runs as uid:gid 65532:65532.
# Pin to a digest in production, e.g.:
#   FROM gcr.io/distroless/static-debian12:nonroot@sha256:<digest> AS runtime
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# Re-declare ARGs needed for labels (ARGs do not cross stages).
ARG VERSION=dev
ARG GIT_SHA=unknown
ARG BUILD_DATE=unknown

# OCI labels → provenance for `docker inspect`, scanners, and registries.
LABEL org.opencontainers.image.title="powerhouse-cache" \
  org.opencontainers.image.description="From-scratch, RESP-compatible, durable in-memory cache (data plane)." \
  org.opencontainers.image.source="https://github.com/Deepesh123455/Strata" \
  org.opencontainers.image.documentation="https://github.com/Deepesh123455/Strata/blob/main/docs/DEPLOYMENT.md" \
  org.opencontainers.image.version="${VERSION}" \
  org.opencontainers.image.revision="${GIT_SHA}" \
  org.opencontainers.image.created="${BUILD_DATE}" \
  org.opencontainers.image.vendor="Strata" \
  org.opencontainers.image.licenses="MIT"

# WORKDIR=/ so the process CWD makes the relative walPath "./data/powerhouse.wal"
# resolve to "/data/powerhouse.wal" — i.e. onto the mounted volume. This is the
# single correctness pin called out in DEPLOYMENT.md: if CWD and the mount don't
# line up, durability silently writes to the ephemeral layer and is lost.
WORKDIR /

COPY --from=build /out/powerhoused /powerhoused
COPY --from=build /out/healthcheck /healthcheck
# Empty, correctly-owned WAL dir → a fresh named volume inherits 65532:65532.
COPY --from=build --chown=65532:65532 /out/data /data

# The durable WAL lives here; mount an EBS volume (Topology A) or EFS access
# point (Topology B) at /data in production so it outlives the container.
VOLUME ["/data"]

EXPOSE 6379
USER nonroot:nonroot

# Container-level liveness WITHOUT a shell: the baked-in probe opens a RESP
# connection and PINGs. Orchestrators (compose/ECS/k8s) can layer their own
# checks on top; this makes a bare `docker run` self-reporting too.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/healthcheck", "-addr", "127.0.0.1:6379"]

# Default args can be overridden at run time, e.g. `-maxmemory 700` on a 1 GB
# box, or POWERHOUSE_MAXMEMORY_MB via the environment.
ENTRYPOINT ["/powerhoused"]
