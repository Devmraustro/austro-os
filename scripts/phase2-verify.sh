#!/bin/sh
# Phase 2 verification gate (ADR-012). Reproduces the Phase 2 CI commands
# against the local docker-compose network. It never replaces or weakens the
# Phase 1 gate (ROADMAP §10.6).
#
# Usage:
#   scripts/phase2-verify.sh
#
# Assumes: the AUSTRO-OS compose stack is up (postgres/redis/rabbitmq/api) and
# the Go toolchain is available via the golang:1.25.13-alpine image with the
# pre-warmed module cache on the named volume `gomodcache`.
set -u

NET="austro-os_austro_net"
IMG="golang:1.25.13-alpine"
REPO="$(pwd)"

export GOFLAGS=-mod=mod
export GOTOOLCHAIN=local
export GOPROXY=off

ENV="
-e GOFLAGS=-mod=mod -e GOTOOLCHAIN=local -e GOPROXY=off
-e AUSTRO_ENV=development
-e AUSTRO_POSTGRES_DSN=postgres://austro:austro@postgres:5432/austro?sslmode=disable
-e AUSTRO_REDIS_ADDR=redis:6379
-e AUSTRO_RABBITMQ_URL=amqp://austro:austro@rabbitmq:5672
-e AUSTRO_JWT_SECRET=austro-dev-jwt-secret-key-min-32-chars
-e AUSTRO_JWT_REFRESH_SECRET=austro-dev-jwt-refresh-secret-min-32-chars
-e AUSTRO_API_URL=http://api:8080
"

run() {
  docker run --rm --network "$NET" -v "$REPO":/src -v gomodcache:/go/pkg/mod \
    $ENV -w /src "$IMG" sh -c "$1"
}

echo "=== Phase 2 verification started ==="

echo "--- go build ./... ---"
run 'go build ./...' || { echo "BUILD FAILED"; exit 1; }
echo "BUILD_OK"

echo "--- go vet ./... ---"
run 'go vet ./...' || { echo "VET FAILED"; exit 1; }
echo "VET_OK"

echo "--- unit tests (internal) ---"
run 'go test ./internal/... -count=1' || { echo "UNIT FAILED"; exit 1; }
echo "UNIT_OK"

echo "--- full stack regression (tests suite) ---"
run 'go test ./tests/ -count=1 -timeout 600s' || { echo "REGRESSION FAILED"; exit 1; }
echo "REGRESSION_OK"

echo "PHASE 2 STATUS: VERIFY_PASS"
exit 0