#!/usr/bin/env bash
# Authoritative release build.
#
# Single source of truth for build identity, shared by CI, the Dockerfile and
# local release builds. Every released binary must be produced by this script so
# the ldflags cannot drift between call sites.
#
# Usage:
#   scripts/build-release.sh <outdir> [arch] [version]
#
#   outdir   directory to write austro-api / austro-worker into (created)
#   arch     GOARCH, default amd64
#   version  release version, default 1.0 (the value already declared in the
#            Dockerfile OCI label and previously hardcoded in the startup log)
#
# Identity rules, and why each is here:
#
#   -trimpath      removes local filesystem paths so the build does not encode
#                  the directory it ran in. Without it two builds of the same
#                  commit differ.
#   -s -w          drops the symbol table and DWARF. No profiling or debug
#                  endpoint is exposed, so neither is needed in a release.
#   Commit         resolved with `git rev-parse HEAD` and injected as the exact
#                  full SHA. This is the SOURCE commit.
#   No build date   deliberately not injected. A timestamp would differ between
#                  two builds of the same commit and destroy reproducibility.
#                  Per-build metadata belongs in the release manifest, which is
#                  written outside the repository.
set -euo pipefail

OUTDIR=${1:?usage: build-release.sh <outdir> [arch] [version]}
ARCH=${2:-amd64}
VERSION=${3:-1.0}

PKG="austro-os/internal/buildinfo"

# A dirty tree is refused rather than silently stamped. A release artifact that
# reports a commit but was built from modified sources is exactly the provenance
# lie this package exists to prevent. Use --allow-dirty for local experiments
# only; the result will still report the commit of HEAD, which is then wrong.
if [ "${ALLOW_DIRTY:-0}" != "1" ]; then
  if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
    echo "error: tracked files are modified; refusing to stamp a release build with HEAD's SHA" >&2
    echo "       commit the change, or set ALLOW_DIRTY=1 for a local build." >&2
    exit 1
  fi
fi

COMMIT=$(git rev-parse HEAD)
TREE=$(git rev-parse HEAD^{tree})
LDFLAGS="-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.Commit=${COMMIT}"

mkdir -p "$OUTDIR"

echo "building austro-api and austro-worker"
echo "  version  : ${VERSION}"
echo "  commit   : ${COMMIT}"
echo "  tree     : ${TREE}"
echo "  arch     : ${ARCH}"
echo "  ldflags  : ${LDFLAGS}"

# Both binaries are built from the same source with the same flags, so the two
# processes in one deployment can never report different identities.
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags "$LDFLAGS" -o "$OUTDIR/austro-api"  .
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags "$LDFLAGS" -o "$OUTDIR/austro-worker" ./cmd/worker

echo "  built    : $OUTDIR/austro-api"
echo "  built    : $OUTDIR/austro-worker"
