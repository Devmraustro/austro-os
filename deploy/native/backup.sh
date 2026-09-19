#!/usr/bin/env bash
#
# AUSTRO OS — native ARM64 PostgreSQL backup.
#
# The deployment/native counterpart of scripts/backup.sh. Dumps the
# authoritative datastore, compresses it, proves the artifact is a usable dump
# rather than a truncated file, and optionally copies it off-host.
#
#   sudo deploy/native/backup.sh
#
# Why only PostgreSQL:
#   PostgreSQL is the system of record. Redis holds workspace memory that is
#   rebuildable from it, and RabbitMQ holds transient in-flight messages. A
#   backup that treated those as authoritative would be documenting a recovery
#   guarantee the system does not actually make. See
#   docs/backups-and-restore.md.
#
# Environment:
#   AUSTRO_ENV_FILE                  default /etc/austro/austro.env
#   AUSTRO_BACKUP_DIR                default /var/backups/austro
#   AUSTRO_BACKUP_RETENTION_DAYS     default 14 (0 disables pruning)
#   AUSTRO_BACKUP_S3_BUCKET          unset by default; enables the off-host copy
#   AUSTRO_BACKUP_S3_PREFIX          default austro-os/
#   AUSTRO_BACKUP_S3_REGION          optional, passed to the AWS CLI
#
# No secret is printed by this script. The dump runs over the local PostgreSQL
# unix socket as the postgres OS user, authenticated by peer (no password), so
# the database credential never enters this process's environment, its
# argument list or its output.

set -Eeuo pipefail

ENV_FILE="${AUSTRO_ENV_FILE:-/etc/austro/austro.env}"
BACKUP_DIR="${AUSTRO_BACKUP_DIR:-/var/backups/austro}"
RETENTION_DAYS="${AUSTRO_BACKUP_RETENTION_DAYS:-14}"
S3_BUCKET="${AUSTRO_BACKUP_S3_BUCKET:-}"
S3_PREFIX="${AUSTRO_BACKUP_S3_PREFIX:-austro-os/}"

log() { printf '[backup] %s\n' "$*"; }
die() { printf '[backup] ERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
	die "run as root (sudo): the dump must run as the postgres OS user over the local socket"
fi
command -v pg_dump >/dev/null 2>&1 || die "pg_dump is not installed or not on PATH"
command -v gzip >/dev/null 2>&1 || die "gzip is not installed or not on PATH"
[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE"

set -a
# shellcheck source=/etc/austro/austro.env
# shellcheck disable=SC1091
. "$ENV_FILE"
set +a

DB_NAME="${AUSTRO_POSTGRES_DB:-austro}"

if ! runuser -u postgres -- pg_isready -U postgres -d postgres -h /var/run/postgresql >/dev/null 2>&1; then
	die "PostgreSQL is not accepting connections; refusing to write a partial dump"
fi

install -d -o root -g root -m 0750 "$BACKUP_DIR"
[[ -w "$BACKUP_DIR" ]] || die "backup directory is not writable: $BACKUP_DIR"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="${BACKUP_DIR%/}/austro-postgres-${STAMP}.sql.gz"

# ---------------------------------------------------------------------------
# Dump
# ---------------------------------------------------------------------------
# pg_dump runs as the postgres OS user over the unix socket, authenticated by
# peer. --clean --if-exists makes the artifact self-contained: restoring it
# into an existing database drops and recreates what it owns, instead of
# failing on the first object that already exists.
#
# pipefail is what makes this safe: without it, a pg_dump that died halfway
# would still leave gzip exiting 0 and the script would report success for a
# truncated backup.
log "dumping $DB_NAME over the local socket"
if ! runuser -u postgres -- pg_dump -U postgres -d "$DB_NAME" --clean --if-exists |
	gzip -9 >"$OUT"; then
	rm -f "$OUT"
	die "pg_dump failed; no backup was written"
fi

# ---------------------------------------------------------------------------
# Verify the artifact
# ---------------------------------------------------------------------------
# Three independent checks: non-empty, intact gzip stream, and a PostgreSQL
# dump header. The header check consumes the whole stream rather than closing
# it early, so gzip is never SIGPIPEd into a false failure.
size_bytes="$(wc -c <"$OUT" | tr -d '[:space:]')"
(( size_bytes > 0 )) || { rm -f "$OUT"; die "backup is empty"; }

if ! gzip -t "$OUT" 2>/dev/null; then
	rm -f "$OUT"
	die "backup is not a valid gzip stream"
fi

if ! gzip -dc "$OUT" 2>/dev/null | grep -F 'PostgreSQL database dump' >/dev/null; then
	rm -f "$OUT"
	die "backup does not contain a PostgreSQL dump header"
fi

CHECKSUM="$(sha256sum "$OUT" | cut -d' ' -f1)"
printf '%s  %s\n' "$CHECKSUM" "$(basename "$OUT")" >"${OUT}.sha256"

log "backup written: $OUT"
log "size: $(numfmt --to=iec "$size_bytes" 2>/dev/null || printf '%s bytes' "$size_bytes")"
log "sha256: $CHECKSUM"

# ---------------------------------------------------------------------------
# Optional off-host copy
# ---------------------------------------------------------------------------
# Attempted ONLY when a bucket is explicitly configured. Silence here is not an
# implied pass: the log line either states where the copy went, or states that
# the backup is local-only and therefore on the same failure domain as the
# database it protects.
if [[ -z "$S3_BUCKET" ]]; then
	log "no AUSTRO_BACKUP_S3_BUCKET configured — backup is LOCAL ONLY (same host as the database)"
else
	command -v aws >/dev/null 2>&1 || die "AUSTRO_BACKUP_S3_BUCKET is set but the aws CLI is not installed"
	s3_uri="s3://${S3_BUCKET%/}/${S3_PREFIX}"

	aws_args=(s3 cp "$OUT" "$s3_uri")
	[[ -n "${AUSTRO_BACKUP_S3_REGION:-}" ]] && aws_args=(--region "$AUSTRO_BACKUP_S3_REGION" "${aws_args[@]}")

	log "uploading to ${s3_uri}$(basename "$OUT")"
	if ! aws "${aws_args[@]}" >/dev/null; then
		die "S3 upload failed (the local backup at $OUT is intact)"
	fi

	aws_sha_args=(s3 cp "${OUT}.sha256" "$s3_uri")
	[[ -n "${AUSTRO_BACKUP_S3_REGION:-}" ]] && aws_sha_args=(--region "$AUSTRO_BACKUP_S3_REGION" "${aws_sha_args[@]}")
	aws "${aws_sha_args[@]}" >/dev/null || die "S3 upload of the checksum failed"

	log "off-host copy complete: ${s3_uri}$(basename "$OUT")"
fi

# ---------------------------------------------------------------------------
# Retention
# ---------------------------------------------------------------------------
# Only files this script itself produces, matched by name, so a prune can never
# delete something an operator placed in the same directory.
if [[ "$RETENTION_DAYS" =~ ^[0-9]+$ ]] && (( RETENTION_DAYS > 0 )); then
	pruned=0
	while IFS= read -r -d '' old; do
		rm -f "$old"
		pruned=$((pruned + 1))
	done < <(find "$BACKUP_DIR" -maxdepth 1 -type f \
		\( -name 'austro-postgres-*.sql.gz' -o -name 'austro-postgres-*.sql.gz.sha256' \) \
		-mtime "+${RETENTION_DAYS}" -print0 2>/dev/null)

	log "retention: removed $pruned file(s) older than ${RETENTION_DAYS} day(s)"
else
	log "retention: pruning disabled (AUSTRO_BACKUP_RETENTION_DAYS=$RETENTION_DAYS)"
fi

log "backup complete"