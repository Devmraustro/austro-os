# syntax=docker/dockerfile:1
#
# AUSTRO OS runtime image.
#
# One image carries both long-running processes of the supported topology
# (docs/production-deployment.md):
#
#   API     -> /app/main    (CMD, the default this image runs)
#   Worker  -> /app/worker  (selected by the compose `command` override)
#
# They ship from a single artifact so the two processes can never drift onto
# different revisions of the same codebase, and they are started as separate
# containers with separate lifetimes, exactly as the production topology
# requires. Nothing here changes what the application does; the image only
# packages the existing entrypoints.

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
# Pinned to the same Go release the CI workflows pin (go-version 1.25.13), so a
# binary built here is built by the same toolchain the test suite validated.
FROM golang:1.25.13-alpine AS builder

# git is required for `go mod download` to resolve VCS-hosted modules.
RUN apk add --no-cache git

# GOTOOLCHAIN=local refuses to silently download a different toolchain than the
# one pinned above, which is what keeps the "pinned version" claim true rather
# than nominal.
ENV GOTOOLCHAIN=local
ENV GOFLAGS=-mod=mod
# CGO is disabled for the whole build: the binaries are statically linked, which
# is what lets them run on a minimal runtime image with no libc shims.
ENV CGO_ENABLED=0

WORKDIR /app

# Dependency layer first so a source-only change does not re-download modules.
COPY go.mod go.sum ./
COPY *.go ./
COPY internal ./internal
COPY infrastructure ./infrastructure
COPY cmd ./cmd

# Bounded retry: a transient proxy failure should not fail a release build, but
# it must still fail rather than produce an image with missing modules.
RUN for i in $(seq 1 10); do go mod download && exit 0 || sleep 2; done; exit 1

# -trimpath removes local build paths from the binary; -s -w drops the symbol
# table and DWARF data. Neither is needed by a released artifact, and the
# project exposes no profiling or debug endpoints.
RUN GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/main . \
    && GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
# Pinned minor line. The runtime image carries no compiler and no shell
# toolchain: only the two binaries, the CA root store and busybox.
FROM alpine:3.19

LABEL org.opencontainers.image.title="AUSTRO OS" \
      org.opencontainers.image.description="AUSTRO OS API and worker runtime (modular monolith)" \
      org.opencontainers.image.source="https://github.com/Devmraustro/austro-os" \
      org.opencontainers.image.vendor="AUSTRO" \
      org.opencontainers.image.version="1.0"

# ca-certificates is required, not cosmetic: the AI gateway and the generic
# HTTP publisher make outbound HTTPS calls, and the Alpine base image ships no
# root store, so every such call fails certificate verification.
#
# The runtime user is created with an explicit, fixed uid/gid so the identity is
# stable across rebuilds and can be referenced by host-side policy without
# depending on whichever id `adduser -S` happened to allocate.
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 appgroup \
    && adduser -S -u 10001 -G appgroup -H -s /sbin/nologin appuser

WORKDIR /app

# --chown keeps the files owned by the runtime user; the binaries are copied
# from the build stage already executable (go build emits 0755).
COPY --from=builder --chown=appuser:appgroup /out/main /app/main
COPY --from=builder --chown=appuser:appgroup /out/worker /app/worker

# Non-root for the whole container lifetime. The image needs no write access
# anywhere on disk: logs go to stdout/stderr as JSON, so nothing is written
# into the image filesystem and a read-only rootfs remains possible.
USER appuser

# Documentation only — the API listens on 8080; the worker opens no socket.
EXPOSE 8080

# Explicit SIGTERM so `docker stop` reaches the graceful drain rather than
# killing the process. Both entrypoints install a SIGINT/SIGTERM handler; the
# API drains in-flight requests (up to 75s) before exiting.
STOPSIGNAL SIGTERM

# Liveness probe for the API process. busybox wget is part of the Alpine base
# image and is what performs this check, so no extra package is pulled in.
#
# start-period is deliberately generous: on a cold database the first boot also
# applies schema migrations and provisions the RLS role topology before the
# listener is opened, which is slower than a warm restart.
#
# The worker container overrides this healthcheck in docker-compose.production.yml
# (it serves no HTTP, so an HTTP probe would report a permanently unhealthy
# container that is in fact working correctly).
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/health/live || exit 1

# Exec form: PID 1 is the binary itself, so signals are delivered to the
# application's own handler instead of to a shell that would swallow them.
CMD ["./main"]
