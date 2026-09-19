#!/usr/bin/env bash
#
# AUSTRO OS — native ARM64 PostgreSQL restore.
#
# The deployment/native counterpart of scripts/restore.sh.
#
#   sudo deploy/native/restore.sh <backup.sql.gz>
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
# re-establishes them for the restored tables.

set -Eeuo pipefail

ENV_FILE="${AUSTRO_ENV_FILE:-/etc/austro/austro.env}"
HEALTHCHECK="${AUSTRO_HEALTHCHECK:-/opt/austro/deploy/native/healthcheck.sh}"
BACKUP_FILE="${1:-}"

log()  { printf '[restore] %s\n' "$*"; }
warn() { printf '[restore] WARNING: %s\n' "$*" >&2; }
die()  { printf '[restore] ERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
	die "run as root (sudo)"
fi
command -v psql >/dev/null 2>&1 || die "psql is not installed or not on PATH"
[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE"
[[ -n "$BACKUP_FILE" ]] || die "usage: deploy/native/restore.sh <backup.sql.gz>"
[[ -f "$BACKUP_FILE" ]] || die "backup file not found: $BACKUP_FILE"

set -a
# shellcheck source=/etc/austro/austro.env
# shellcheck disable=SC1091
. "$ENV_FILE"
set +a

DB_NAME="${AUSTRO_POSTGRES_DB:-austro}"
DB_RUNTIME_USER="${AUSTRO_POSTGRES_RUNTIME_USER:-austro_app}"
DB_RUNTIME_PASSWORD="${AUSTRO_POSTGRES_RUNTIME_PASSWORD:-}"

# ---------------------------------------------------------------------------
# 1. Validate the artifact BEFORE anything is touched
# ---------------------------------------------------------------------------
size_bytes="$(wc -c <"$BACKUP_FILE" | tr -d '[:space:]')"
(( size_bytes > 0 )) || die "backup file is empty: $BACKUP_FILE"

gzip -t "$BACKUP_FILE" 2>/dev/null || die "backup is not a valid gzip stream: $BACKUP_FILE"
# Full-stream consumer, not `head | grep -q`: an early-closing consumer SIGPIPEs
# gzip after the match, and pipefail then reports gzip's 141.
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
log "stopping worker and api"
systemctl stop austro-worker.service 2>/dev/null || true
systemctl stop austro-api.service 2>/dev/null || true

for service in austro-worker.service austro-api.service; do
	state="$(systemctl is-active "$service" 2>/dev/null || echo inactive)"
	[[ "$state" == "inactive" || "$state" == "failed" ]] \
		|| die "$service is still '$state' — refusing to restore while the application can write"
done
log "application stopped"

# ---------------------------------------------------------------------------
# 4. Restore
# ---------------------------------------------------------------------------
# Highlights over the local socket as the postgres OS user (peer). ON_ERROR_STOP
# turns the first failing statement into a hard failure.
log "restoring into $DB_NAME"
if ! gzip -dc "$BACKUP_FILE" | runuser -u postgres -- psql -v ON_ERROR_STOP=1 -U postgres -d "$DB_NAME"; then
	die "restore failed — the database may be in a partial state; inspect the output above before continuing"
fi
log "restore statements completed"

# ---------------------------------------------------------------------------
# 5. Re-establish the application topology
# ---------------------------------------------------------------------------
# Starting the API re-applies the idempotent bootstrap (schema migrations, role
# provisioning, RLS policies, forced RLS) against the restored schema.
log "starting api to re-apply the schema bootstrap"
systemctl start austro-api.service

wait_for_api() {
	local timeout="${1:-420}" waited=0 interval=3
	while (( waited < timeout )); do
		if curl -fsS http://127.0.0.1:8080/health/live >/dev/null 2>&1; then
			return 0
		fi
		sleep "$interval"
		waited=$((waited + interval))
	done
	printf '[restore] ERROR: the API did not come up against the restored database within %ss\n' "$timeout" >&2
	journalctl --no-pager -n 300 -u austro-api.service --tail 60 >&2 || true
	return 1
}

wait_for_api 420 || die "the API did not come up against the restored database"

# ---------------------------------------------------------------------------
# 6. Verify the restored system
# ---------------------------------------------------------------------------
VERIFY_PASS=0
VERIFY_FAIL=0
ok()  { printf 'PASS  %s\n' "$1"; VERIFY_PASS=$((VERIFY_PASS + 1)); }
bad() { printf 'FAIL  %s\n' "$1" >&2; VERIFY_FAIL=$((VERIFY_FAIL + 1)); }

psql_owner() { runuser -u postgres -- psql -U postgres -d "$DB_NAME" -tAc "$1" 2>/dev/null | tr -d '[:space:]' || true; }

printf '\n--- verification ---\n'

table_count="$(psql_owner "
SELECT count(*) FROM information_schema.tables
 WHERE table_schema='public'
   AND table_name IN ('workspaces','departments','teams','ai_employees','ceos','tasks',
                      'knowledge_documents','memory_embeddings','publications','pipelines',
                      'users','audit_events');")"
if [[ "$table_count" == "12" ]]; then
	ok "all 12 protected tables are present"
else
	bad "expected 12 protected tables, found ${table_count:-<none>}"
fi

vector_ext="$(psql_owner "SELECT count(*) FROM pg_extension WHERE extname='vector';")"
if [[ "$vector_ext" == "1" ]]; then
	ok "pgvector extension is installed"
else
	bad "pgvector extension is missing"
fi

rls_enabled="$(PGPASSWORD="$DB_RUNTIME_PASSWORD" psql \
	-U "$DB_RUNTIME_USER" -d "$DB_NAME" -tAc \
	"SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relrowsecurity;" 2>/dev/null | tr -d '[:space:]' || true)"
rls_forced="$(PGPASSWORD="$DB_RUNTIME_PASSWORD" psql \
	-U "$DB_RUNTIME_USER" -d "$DB_NAME" -tAc \
	"SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relforcerowsecurity;" 2>/dev/null | tr -d '[:space:]' || true)"

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

runtime_security="$(psql_owner "
SELECT rolsuper::text || ',' || rolbypassrls::text
  FROM pg_roles WHERE rolname = '$DB_RUNTIME_USER';")"
if [[ "$runtime_security" == "false,false" ]]; then
	ok "runtime role $DB_RUNTIME_USER is not a superuser and does not hold BYPASSRLS"
else
	bad "runtime role $DB_RUNTIME_USER has elevated privileges (superuser,bypassrls = ${runtime_security:-<not found>})"
fi

owned_tables="$(psql_owner "
SELECT count(*) FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_roles r ON r.oid = c.relowner
 WHERE n.nspname='public' AND c.relkind='r' AND r.rolname='$DB_RUNTIME_USER';")"
if [[ "$owned_tables" == "0" ]]; then
	ok "runtime role owns no protected table (ownership separation intact)"
else
	bad "runtime role owns ${owned_tables:-<unknown>} protected table(s); RLS does not apply to an owner"
fi

runtime_login="$(PGPASSWORD="$DB_RUNTIME_PASSWORD" psql \
	-U "$DB_RUNTIME_USER" -d "$DB_NAME" -tAc 'SELECT 1' 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$runtime_login" == "1" ]]; then
	ok "the runtime role can authenticate against the restored database"
else
	bad "the runtime role could not authenticate against the restored database"
fi

audit_shape="$(psql_owner "
SELECT count(*) FROM information_schema.columns
 WHERE table_schema='public' AND table_name='audit_events'
   AND column_name IN ('seq','workspace_id','timestamp_canonical');")"
if [[ "$audit_shape" == "3" ]]; then
	ok "persistent audit schema is intact (seq, workspace_id, timestamp_canonical)"
else
	bad "persistent audit schema is incomplete (found ${audit_shape:-0} of 3 expected columns)"
fi

audit_rows="$(psql_owner "SELECT count(*) FROM audit_events;")"
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
systemctl start austro-worker.service

if ! systemctl is-active --quiet austro-worker.service; then
	wait_timeout=180 waited=0
	while (( waited < wait_timeout )); do
		if systemctl is-active --quiet austro-worker.service; then
			break
		fi
		sleep 3
		waited=$((waited + 3))
	done
	systemctl is-active --quiet austro-worker.service \
		|| die "the worker did not come up against the restored database (see journalctl -u austro-worker)"
fi

log "running healthcheck"
[[ -f "$HEALTHCHECK" ]] || die "healthcheck script is missing: $HEALTHCHECK"
bash "$HEALTHCHECK" || die "healthcheck failed after restore"

log "restore complete"