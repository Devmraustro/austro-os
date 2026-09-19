#!/usr/bin/env bash
# install-release.sh — install (or update) a AUSTRO OS native release on this
# host, with integrity verification, an atomic switch, a health gate and
# automatic rollback.
#
# Usage:
#   sudo deploy/native/install-release.sh <release-archive.tar.gz> <expected-sha256>
#
# The archive must unpack to exactly two executables, `main` and `worker`
# (the AArch64 builds produced by .github/workflows/native-arm64-deploy.yml).
# It is installed under /opt/austro/releases/<expected-sha256>/ and the
# /opt/austro/current symlink is switched to it atomically (same filesystem,
# via a .new temporary name). After the switch the API is restarted and polled
# on /health/live, then the worker is restarted and the full shallow-to-deep
# healthcheck (deploy/native/healthcheck.sh) must pass. Any failure switches
# back to the previous release, restarts both units, and exits non-zero.
#
# The expected SHA-256 is the SAME value that gates the CI deploy job (the
# checksum computed at build time), so release integrity is established twice
# before the switch: here by the operator-supplied value, and in CI by the
# workflow's own verification step.
#
# Old releases are pruned automatically, keeping only the current and the
# immediately previous one so a rollback always has its binary in place.

set -Eeuo pipefail

ENV_FILE="${AUSTRO_ENV_FILE:-/etc/austro/austro.env}"
RELEASES_DIR=/opt/austro/releases
CURRENT=/opt/austro/current
API_URL="${AUSTRO_API_URL:-http://127.0.0.1:8080/health/live}"
HEALTHCHECK="${AUSTRO_HEALTHCHECK:-/opt/austro/deploy/native/healthcheck.sh}"
HEALTH_WAIT_SECS="${AUSTRO_HEALTH_WAIT_SECS:-60}"
HEALTH_POLL_SECS="${AUSTRO_HEALTH_POLL_SECS:-2}"

log() { printf 'install-release: %s\n' "$*" >&2; }

die() {
	log "error: $*"
	exit 1
}

if [ "$#" -ne 2 ]; then
	die "usage: install-release.sh <release.tar.gz> <expected-sha256>"
fi

ARCHIVE="$1"
EXPECTED_SHA="${2,,}"

if [ "$(id -u)" -ne 0 ]; then
	die "run as root (sudo)"
fi

[ -f "$ARCHIVE" ] || die "release archive not found: $ARCHIVE"
printf '%s' "$EXPECTED_SHA" | grep -Eq '^[0-9a-f]{64}$' || die "expected SHA-256 must be 64 lowercase hex characters"

if ! command -v sha256sum >/dev/null 2>&1; then
	die "sha256sum is required"
fi
if ! command -v curl >/dev/null 2>&1; then
	die "curl is required (bootstrap-ubuntu24.sh installs it)"
fi
if [ ! -r "$ENV_FILE" ]; then
	die "cannot read $ENV_FILE — create it first (see deploy/native/generate-env.sh)"
fi

# Integrity is verified against the operator-supplied checksum before anything
# on disk is touched.
ACTUAL_SHA="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
	die "checksum mismatch: archive is $ACTUAL_SHA, expected $EXPECTED_SHA — refusing to install"
fi

RELEASE_DIR="$RELEASES_DIR/$EXPECTED_SHA"

# Previous release for rollback, captured BEFORE any change. On first install
# CURRENT is the placeholder directory bootstrap created; on every later
# install it is a symlink to a releases/<sha> directory.
PREV=""
if [ -L "$CURRENT" ]; then
	PREV="$(readlink "$CURRENT")"
fi

log "installing release $EXPECTED_SHA (previous: ${PREV:-none})"

install -d -o austro -g austro "$RELEASE_DIR"
if [ -e "$RELEASE_DIR/main" ] && [ -e "$RELEASE_DIR/worker" ]; then
	log "release directory already populated; refreshing binaries"
fi
tar -xzf "$ARCHIVE" -C "$RELEASE_DIR"
chown -R austro:austro "$RELEASE_DIR"
chmod 0755 "$RELEASE_DIR/main" "$RELEASE_DIR/worker"

[ -x "$RELEASE_DIR/main" ] || die "$RELEASE_DIR/main missing or not executable after extraction"
[ -x "$RELEASE_DIR/worker" ] || die "$RELEASE_DIR/worker missing or not executable after extraction"

rollback() {
	log "rolling back to ${PREV:-placeholder}"
	# First install: CURRENT started as a real directory; remove the symlink and
	# restore the placeholder so the units keep failing LOUDLY until a working
	# release lands, rather than pointing at half of a release.
	if [ -n "$PREV" ]; then
		ln -sfn "$PREV" "$CURRENT.new"
		mv -Tf "$CURRENT.new" "$CURRENT"
	else
		rm -f "$CURRENT"
		install -d -o austro -g austro "$CURRENT"
	fi
	systemctl restart austro-api.service austro-worker.service >/dev/null 2>&1 || true
	die "$1"
}

# Atomically switch CURRENT to the new release (both on the same filesystem,
# so the rename is atomic).
ln -sfn "$RELEASE_DIR" "$CURRENT.new"
mv -Tf "$CURRENT.new" "$CURRENT"

log "restarting API and waiting for /health/live"
systemctl restart austro-api.service

API_OK=0
for _ in $(seq 1 $((HEALTH_WAIT_SECS / HEALTH_POLL_SECS))); do
	if curl -fsS "$API_URL" >/dev/null 2>&1; then
		API_OK=1
		break
	fi
	sleep "$HEALTH_POLL_SECS"
done
[ "$API_OK" -eq 1 ] || rollback "API did not become healthy on $API_URL within ${HEALTH_WAIT_SECS}s (see journalctl -u austro-api)"


log "restarting worker"
systemctl restart austro-worker.service

if ! systemctl is-active --quiet austro-worker.service; then
	rollback "worker unit not active after restart (see journalctl -u austro-worker)"
fi

log "running healthcheck"
if ! bash "$HEALTHCHECK"; then
	rollback "healthcheck failed after release $EXPECTED_SHA"
fi

log "release $EXPECTED_SHA is live and healthy"

# Prune: keep the current release and its immediate predecessor only.
if [ -n "$PREV" ]; then
	find "$RELEASES_DIR" -mindepth 1 -maxdepth 1 -type d ! -name "$EXPECTED_SHA" ! -name "$(basename "$PREV")" -exec rm -rf {} +
else
	find "$RELEASES_DIR" -mindepth 1 -maxdepth 1 -type d ! -name "$EXPECTED_SHA" -exec rm -rf {} +
fi

log "done: /opt/austro/current → $EXPECTED_SHA"