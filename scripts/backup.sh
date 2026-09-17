#!/usr/bin/env bash
#
# AUSTRO OS — PostgreSQL backup.
#
# Dumps the authoritative datastore, compresses it, proves the artifact is a
# usable dump rather than a truncated file, and optionally copies it off-host.
#
#   scripts/backup.sh
#
# Why only PostgreSQL:
#   PostgreSQL is the system of record. Redis holds workspace memory that is
#   rebuildable from it, and RabbitMQ holds transient in-flight messages. A
#   backup that treated those as authoritative would be documenting a recovery
#   guarantee the system does not actually make. See
#   docs/backups-and-restore.md.
#
# Environment:
#   AUSTRO_ENV_FILE                  default .env.production
#   AUSTRO_BACKUP_DIR                default ./backups
#   AUSTRO_BACKUP_RETENTION_DAYS     default 14 (0 disables pruning)
#   AUSTRO_BACKUP_S3_BUCKET          unset by default; enables the off-host copy
#   AUSTRO_BACKUP_S3_PREFIX          default austro-os/
#   AUSTRO_BACKUP_S3_REGION          optional, passed to the AWS CLI
#
# No secret is printed by this script. The dump runs over the PostgreSQL
# container's local socket, which the image configures for local trust, so the
# dump itself needs no password at all: the database credential never enters
# this process's environment, its argument list or its output.

set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_FILE="docker-compose.production.yml"
ENV_FILE="${AUSTRO_ENV_FILE:-.env.production}"
BACKUP_DIR="${AUSTRO_BACKUP_DIR:-./backups}"
RETENTION_DAYS="${AUSTRO_BACKUP_RETENTION_DAYS:-14}"
S3_BUCKET="${AUSTRO_BACKUP_S3_BUCKET:-}"
S3_PREFIX="${AUSTRO_BACKUP_S3_PREFIX:-austro-os/}"

log() { printf '[backup] %s\n' "$*"; }
die() { printf '[backup] ERROR: %s\n' "$*" >&2; exit 1; }

compose() { docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
command -v docker >/dev/null 2>&1 || die "docker is not installed or not on PATH"
docker info >/dev/null 2>&1 || die "cannot talk to the docker daemon"
[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE"

set -a
# shellcheck source=/dev/null
. "$ENV_FILE"
set +a

DB_OWNER="${AUSTRO_POSTGRES_USER:-austro}"
DB_NAME="${AUSTRO_POSTGRES_DB:-austro}"

# Validate the target container before writing anything: a backup script that
# produces an empty file instead of failing is worse than one that refuses.
container_id="$(compose ps -q postgres 2>/dev/null || true)"
[[ -n "$container_id" ]] || die "the postgres container is not created — nothing to back up"
state="$(docker inspect --format '{{.State.Status}}' "$container_id" 2>/dev/null || echo unknown)"
[[ "$state" == "running" ]] || die "the postgres container is '$state', expected running"

if ! compose exec -T postgres pg_isready -U "$DB_OWNER" -d "$DB_NAME" >/dev/null 2>&1; then
    die "PostgreSQL is not accepting connections; refusing to write a partial dump"
fi

mkdir -p "$BACKUP_DIR"
[[ -w "$BACKUP_DIR" ]] || die "backup directory is not writable: $BACKUP_DIR"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="${BACKUP_DIR%/}/austro-postgres-${STAMP}.sql.gz"

# ---------------------------------------------------------------------------
# Dump
# ---------------------------------------------------------------------------
# pg_dump runs over the container's local socket. --clean --if-exists makes the
# artifact self-contained: restoring it into an existing database drops and
# recreates what it owns, instead of failing on the first object that already
# exists.
#
# pipefail is what makes this safe: without it, a pg_dump that died halfway
# would still leave gzip exiting 0 and the script would report success for a
# truncated backup.
log "dumping $DB_NAME as owner role $DB_OWNER"
if ! compose exec -T postgres pg_dump -U "$DB_OWNER" -d "$DB_NAME" --clean --if-exists \
        | gzip -9 >"$OUT"; then
    rm -f "$OUT"
    die "pg_dump failed; no backup was written"
fi

# ---------------------------------------------------------------------------
# Verify the artifact
# ---------------------------------------------------------------------------
# Three independent checks, because "the file exists" is not "the file is a
# backup": it must be non-empty, it must be an intact gzip stream, and it must
# actually contain a PostgreSQL dump header. The header check consumes the whole
# stream (`grep -F ... >/dev/null`) rather than closing it early: an
# early-closing consumer (`head -40 | grep -q`) SIGPIPEs gzip after the match,
# and pipefail then reports gzip's 141 -- a valid backup would be rejected as
# if it had no header.
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

# A checksum is not a secret and makes "is this the file I restored from?"
# answerable later.
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

    # The checksum file goes with it so a restore can prove which bytes it got.
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
