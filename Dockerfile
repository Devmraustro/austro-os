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
#
# Build identity is passed in rather than derived, because this build stage has
# no .git directory: the source is COPYed in, so `git rev-parse` cannot run here.
# The caller must supply the commit it actually built. `docker-compose
# .production.yml` refuses to build without AUSTRO_BUILD_COMMIT; a developer
# running the plain development compose gets the "unknown" default, which the
# binaries then report honestly in `--version` instead of claiming a precision
# they do not have.
ARG VERSION="1.0"
ARG COMMIT="unknown"

# Validate before building, so a bad value fails with a readable message instead
# of a confusing "does not embed" error from three lines later.
#
# Accept exactly two shapes: a full 40-character lowercase hex SHA, or the
# literal "unknown" for an un-stamped local build. Everything else is refused.
# The gap this closes is the misleading middle: COMMIT=main, COMMIT=HEAD, a
# truncated SHA, or an empty value would each produce an image that looks
# properly stamped and that nobody can trace back to a source revision.
RUN set -eu; \
    if [ -z "$VERSION" ]; then \
        echo "VERSION must not be empty; pass --build-arg VERSION=1.0" >&2; exit 1; \
    fi; \
    if [ "$COMMIT" = "unknown" ]; then \
        echo "WARNING: building without build identity; binaries will report version=$VERSION commit=unknown" >&2; \
    elif ! printf '%s' "$COMMIT" | grep -Eq '^[0-9a-f]{40}$'; then \
        echo "COMMIT must be the full 40-character git SHA of the release commit, or the literal 'unknown'." >&2; \
        echo "  got: '$COMMIT'" >&2; \
        echo "  pass it explicitly, e.g. --build-arg COMMIT=\$(git rev-parse HEAD)" >&2; \
        exit 1; \
    fi

# -s -w is kept ahead of the -X flags; the linker accepts them in any order, but
# grouping the symbol stripping with the original flags makes the change to this
# line obvious in review.
#
# The two greps below fail the image build when the binaries do not carry the
# requested commit. Without them a mistyped -X symbol path yields a correctly
# shaped image whose binaries silently report dev/unknown.
#
# Only the COMMIT is asserted here, and that is deliberate. A 40-character hex
# string is unique enough that a match is meaningful. The version ("1.0") is not:
# a three-character token like that occurs all over a Go binary, so grepping for
# it would be a check that can never fail, which is worse than no check at all.
# The version is asserted where it is actually meaningful -- by executing the
# binary's own --version command, in production-deployment.yml and phase2-ci.yml.
# That check also requires an amd64 host to run the amd64 binary this stage
# produces, which a cross-platform image build cannot promise, hence the split.
#
# `grep -a` is used rather than `strings`: busybox grep is guaranteed present in
# the base image, whereas the `strings` applet is not guaranteed in every
# golang:*-alpine variant, and a missing tool here would fail the release build
# for a reason that has nothing to do with the binaries.
RUN set -eux; \
    LDFLAGS="-s -w -X austro-os/internal/buildinfo.Version=${VERSION} -X austro-os/internal/buildinfo.Commit=${COMMIT}"; \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o /out/main .; \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o /out/worker ./cmd/worker; \
    grep -a -qF "$COMMIT" /out/main   || { echo "main does not embed COMMIT=$COMMIT" >&2; exit 1; }; \
    grep -a -qF "$COMMIT" /out/worker || { echo "worker does not embed COMMIT=$COMMIT" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
# Pinned minor line. The runtime image carries no compiler and no shell
# toolchain: only the two binaries, the CA root store and busybox.
FROM alpine:3.19

# An ARG is scoped to the stage that declares it. Without re-declaring these
# here, the labels below would expand to empty strings: the image would carry
# org.opencontainers.image.revision="" while its binaries correctly reported a
# commit, which is precisely the disagreement this change exists to prevent.
ARG VERSION="1.0"
ARG COMMIT="unknown"

LABEL org.opencontainers.image.title="AUSTRO OS" \
      org.opencontainers.image.description="AUSTRO OS API and worker runtime (modular monolith)" \
      org.opencontainers.image.source="https://github.com/Devmraustro/austro-os" \
      org.opencontainers.image.vendor="AUSTRO" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

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
