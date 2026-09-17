#!/usr/bin/env bash
#
# AUSTRO OS — PostgreSQL restore.
#
#   scripts/restore.sh <backup.sql.gz>
#
# This is DESTRUCTIVE. It replaces the contents of the production database with
# the contents of the backup. It refuses to proceed unless
# AUSTRO_RESTORE_CONFIRM=yes is set (or an interactive terminal answers the
# prompt), because an unattended run that wipes a database is not a failure mode
# worth leaving open.
#
# Order, and why:
#   1. validate the artifact        — never start a restore from an unverified file
#   2. stop the worker, then the API — nothing may write while the schema is replaced
#   3. restore
#   4. restart the API              — see the note below
#   5. verify                       — tables, RLS, runtime role, audit
#   6. restart the worker
#   7. healthcheck                  — the same check a deployment runs
#
# Step 4 is not optional. The restore replaces schema objects, which drops the
# row level security policies and grants that were attached to them. The
# application's bootstrap (idempotent, and applied at every startup) is what
# re-establishes them for the restored tables. Verifying RLS before that restart
# would be verifying the dump instead of the restored system — the exact
# confusion this ordering avoids.

set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_FILE="docker-compose.production.yml"
ENV_FILE="${AUSTRO_ENV_FILE:-.env.production}"
BACKUP_FILE="${1:-}"

log()  { printf '[restore] %s\n' "$*"; }
warn() { printf '[restore] WARNING: %s\n' "$*" >&2; }
die()  { printf '[restore] ERROR: %s\n' "$*" >&2; exit 1; }

compose() { docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
command -v docker >/dev/null 2>&1 || die "docker is not installed or not on PATH"
docker info >/dev/null 2>&1 || die "cannot talk to the docker daemon"
[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE"
[[ -n "$BACKUP_FILE" ]] || die "usage: scripts/restore.sh <backup.sql.gz>"
[[ -f "$BACKUP_FILE" ]] || die "backup file not found: $BACKUP_FILE"

set -a
# shellcheck source=/dev/null
. "$ENV_FILE"
set +a

DB_OWNER="${AUSTRO_POSTGRES_USER:-austro}"
DB_NAME="${AUSTRO_POSTGRES_DB:-austro}"
DB_RUNTIME_USER="${AUSTRO_POSTGRES_RUNTIME_USER:-austro_app}"

# ---------------------------------------------------------------------------
# 1. Validate the artifact BEFORE anything is touched
# ---------------------------------------------------------------------------
size_bytes="$(wc -c <"$BACKUP_FILE" | tr -d '[:space:]')"
(( size_bytes > 0 )) || die "backup file is empty: $BACKUP_FILE"

gzip -t "$BACKUP_FILE" 2>/dev/null || die "backup is not a valid gzip stream: $BACKUP_FILE"
# Full-stream consumer, not `head | grep -q`: an early-closing consumer SIGPIPEs
# gzip after the match, and pipefail then reports gzip's 141 -- a valid backup
# would be refused as if it had no header.
gzip -dc "$BACKUP_FILE" 2>/dev/null | grep -F 'PostgreSQL database dump' >/dev/null \
    || die "backup does not contain a PostgreSQL dump header: $BACKUP_FILE"
log "backup artifact validated ($size_bytes bytes)"

if [[ -f "${BACKUP_FILE}.sha256" ]]; then
    expected_sum="$(cut -d' ' -f1 <"${BACKUP_FILE}.sha256" | tr -d '[:space:]')"
    actual_sum="$(sha256sum "$BACKUP_FILE" | cut -d' ' -f1)"
    if [[ "$expected_sum" == "$actual_sum" ]]; then
        log "sha256 matches the checksum written alongside the backup"
    else
        die "checksum mismatch: the backup does not match its .sha256 file (expected $expected_sum, got $actual_sum)"
    fi
else
    warn "no .sha256 file alongside the backup; integrity of the gzip stream was checked but not its identity"
fi

# ---------------------------------------------------------------------------
# 2. Confirm destructively
# ---------------------------------------------------------------------------
warn "this will REPLACE the contents of database '$DB_NAME' and cannot be undone."
warn "every row written since that backup was taken will be lost."

if [[ "${AUSTRO_RESTORE_CONFIRM:-no}" != "yes" ]]; then
    if [[ -t 0 ]]; then
        printf '[restore] type the database name (%s) to confirm: ' "$DB_NAME"
        read -r confirmation
        [[ "$confirmation" == "$DB_NAME" ]] || die "confirmation did not match; nothing was changed"
    else
        die "refusing to restore without confirmation — set AUSTRO_RESTORE_CONFIRM=yes to run non-interactively"
    fi
fi

# ---------------------------------------------------------------------------
# 3. Quiesce the application
# ---------------------------------------------------------------------------
# Worker first: it consumes queue events and writes to the same tables, so
# stopping it second would leave a window where the API is down but the worker
# is still committing. Neither process may be running while objects are dropped
# and recreated underneath it.
container_state() {
    local id
    id="$(compose ps -q "$1" 2>/dev/null || true)"
    [[ -n "$id" ]] && docker inspect --format '{{.State.Status}}' "$id" 2>/dev/null || echo absent
}

log "stopping worker and api"
compose stop worker >/dev/null 2>&1 || true
compose stop api >/dev/null 2>&1 || true

for service in worker api; do
    state="$(container_state "$service")"
    [[ "$state" == "exited" || "$state" == "absent" ]] \
        || die "$service is still '$state' — refusing to restore while the application can write"
done
log "application stopped"

# ---------------------------------------------------------------------------
# 4. Restore
# ---------------------------------------------------------------------------
# ON_ERROR_STOP turns the first failing statement into a hard failure. Without
# it psql would report an error and keep going, leaving a half-restored database
# and an exit status of 0.
log "restoring into $DB_NAME"
if ! gzip -dc "$BACKUP_FILE" | compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "$DB_OWNER" -d "$DB_NAME"; then
    die "restore failed — the database may be in a partial state; inspect the output above before continuing"
fi
log "restore statements completed"

# ---------------------------------------------------------------------------
# 5. Re-establish the application topology
# ---------------------------------------------------------------------------
# Starting the API re-applies the idempotent bootstrap (schema migrations, role
# provisioning, RLS policies, forced RLS) against the restored schema. This is
# the step that makes the restored database a working AUSTRO OS database rather
# than a copy of some tables.
log "starting api to re-apply the schema bootstrap"
compose up -d api

wait_for_healthy() {
    local service="$1" timeout="${2:-300}" waited=0 interval=3 container_id status
    while (( waited < timeout )); do
        container_id="$(compose ps -q "$service" 2>/dev/null || true)"
        if [[ -n "$container_id" ]]; then
            status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container_id" 2>/dev/null || echo unknown)"
            if [[ "$status" == "healthy" ]]; then return 0; fi
        fi
        sleep "$interval"
        waited=$((waited + interval))
    done
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$(compose ps -q "$service" 2>/dev/null || true)" 2>/dev/null || echo unknown)"
    printf '[restore] ERROR: %s did not become healthy within %ss (last status: %s)\n' "$service" "$timeout" "$status" >&2
    compose logs --tail=60 "$service" >&2 || true
    return 1
}

wait_for_healthy api 420 || die "the API did not come up against the restored database"

# ---------------------------------------------------------------------------
# 6. Verify the restored system
# ---------------------------------------------------------------------------
VERIFY_PASS=0
VERIFY_FAIL=0
ok()  { printf 'PASS  %s\n' "$1"; VERIFY_PASS=$((VERIFY_PASS + 1)); }
bad() { printf 'FAIL  %s\n' "$1" >&2; VERIFY_FAIL=$((VERIFY_FAIL + 1)); }

printf '\n--- verification ---\n'

# Tables. The list is the same set the application's bootstrap protects, so a
# restore that silently produced an empty database fails here rather than at the
# first request.
table_count="$(compose exec -T postgres psql -U "$DB_OWNER" -d "$DB_NAME" -tAc "
SELECT count(*) FROM information_schema.tables
 WHERE table_schema='public'
   AND table_name IN ('workspaces','departments','teams','ai_employees','ceos','tasks',
                      'knowledge_documents','memory_embeddings','publications','pipelines',
                      'users','audit_events');" 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$table_count" == "12" ]]; then
    ok "all 12 protected tables are present"
else
    bad "expected 12 protected tables, found ${table_count:-<none>}"
fi

# pgvector, which the memory/knowledge similarity search depends on.
vector_ext="$(compose exec -T postgres psql -U "$DB_OWNER" -d "$DB_NAME" -tAc \
    "SELECT count(*) FROM pg_extension WHERE extname='vector';" 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$vector_ext" == "1" ]]; then
    ok "pgvector extension is installed"
else
    bad "pgvector extension is missing"
fi

# Row level security, checked as the runtime role so it reflects what the
# application can actually do rather than what the owner sees.
rls_query='SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='"'"'public'"'"' AND c.relrowsecurity;'
forced_query='SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='"'"'public'"'"' AND c.relforcerowsecurity;'

rls_enabled="$(compose exec -T -e PGPASSWORD="$AUSTRO_POSTGRES_RUNTIME_PASSWORD" postgres \
    psql -U "$DB_RUNTIME_USER" -d "$DB_NAME" -tAc "$rls_query" 2>/dev/null | tr -d '[:space:]' || true)"
rls_forced="$(compose exec -T -e PGPASSWORD="$AUSTRO_POSTGRES_RUNTIME_PASSWORD" postgres \
    psql -U "$DB_RUNTIME_USER" -d "$DB_NAME" -tAc "$forced_query" 2>/dev/null | tr -d '[:space:]' || true)"

if [[ "$rls_enabled" =~ ^[0-9]+$ ]] && (( rls_enabled >= 12 )); then
    ok "row level security is enabled on $rls_enabled protected tables"
else
    bad "row level security is enabled on ${rls_enabled:-<unknown>} tables (expected at least 12)"
fi

if [[ "$rls_forced" =~ ^[0-9]+$ ]] && (( rls_forced >= 12 )); then
    ok "row level security is FORCED on $rls_forced protected tables"
else
    bad "row level security is forced on ${rls_forced:-<unknown>} tables (expected at least 12)"
fi

# Runtime role security. A superuser, a BYPASSRLS role, or a role owning the
# tables it queries would all make the policies above decorative.
runtime_security="$(compose exec -T postgres psql -U "$DB_OWNER" -d "$DB_NAME" -tAc "
SELECT rolsuper::text || ',' || rolbypassrls::text
  FROM pg_roles WHERE rolname = '$DB_RUNTIME_USER';" 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$runtime_security" == "false,false" ]]; then
    ok "runtime role $DB_RUNTIME_USER is not a superuser and does not hold BYPASSRLS"
else
    bad "runtime role $DB_RUNTIME_USER has elevated privileges (superuser,bypassrls = ${runtime_security:-<not found>})"
fi

owned_tables="$(compose exec -T postgres psql -U "$DB_OWNER" -d "$DB_NAME" -tAc "
SELECT count(*) FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_roles r ON r.oid = c.relowner
 WHERE n.nspname='public' AND c.relkind='r' AND r.rolname='$DB_RUNTIME_USER';" 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$owned_tables" == "0" ]]; then
    ok "runtime role owns no protected table (ownership separation intact)"
else
    bad "runtime role owns ${owned_tables:-<unknown>} protected table(s); RLS does not apply to an owner"
fi

# The runtime credential must work, which is what the application will use.
runtime_login="$(compose exec -T -e PGPASSWORD="$AUSTRO_POSTGRES_RUNTIME_PASSWORD" postgres \
    psql -U "$DB_RUNTIME_USER" -d "$DB_NAME" -tAc 'SELECT 1' 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$runtime_login" == "1" ]]; then
    ok "the runtime role can authenticate against the restored database"
else
    bad "the runtime role could not authenticate against the restored database"
fi

# Audit data. The row count is reported rather than asserted against a floor: a
# legitimately empty organisation has no audit rows, and failing a restore for
# that would be a false alarm. What is asserted is that the persistent-audit
# schema the writer needs survived the restore.
audit_shape="$(compose exec -T postgres psql -U "$DB_OWNER" -d "$DB_NAME" -tAc "
SELECT count(*) FROM information_schema.columns
 WHERE table_schema='public' AND table_name='audit_events'
   AND column_name IN ('seq','workspace_id','timestamp_canonical');" 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$audit_shape" == "3" ]]; then
    ok "persistent audit schema is intact (seq, workspace_id, timestamp_canonical)"
else
    bad "persistent audit schema is incomplete (found ${audit_shape:-0} of 3 expected columns)"
fi

audit_rows="$(compose exec -T postgres psql -U "$DB_OWNER" -d "$DB_NAME" -tAc \
    "SELECT count(*) FROM audit_events;" 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$audit_rows" =~ ^[0-9]+$ ]]; then
    ok "audit_events is readable ($audit_rows row(s) restored)"
    printf '      note: full hash-chain verification is performed by the application at\n'
    printf '            GET /audit/verification, which needs an authenticated founder session.\n'
else
    bad "audit_events could not be read"
fi

printf '\nverification passed: %d  failed: %d\n' "$VERIFY_PASS" "$VERIFY_FAIL"
(( VERIFY_FAIL == 0 )) || die "the restored database failed verification (see the FAIL lines above)"

# ---------------------------------------------------------------------------
# 7. Restart the worker and re-run the full healthcheck
# ---------------------------------------------------------------------------
log "starting worker"
compose up -d worker
wait_for_healthy worker 180 || die "the worker did not come up against the restored database"

log "running healthcheck"
[[ -x scripts/healthcheck.sh ]] || die "scripts/healthcheck.sh is missing or not executable"
scripts/healthcheck.sh || die "healthcheck failed after restore"

log "restore complete"
