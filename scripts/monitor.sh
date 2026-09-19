#!/usr/bin/env bash
#
# AUSTRO OS — operational monitor.
#
# The periodic companion to scripts/healthcheck.sh. The healthcheck verifies a
# topology in depth on demand and reports each check; this monitor emits a
# compact signal set as one JSON line per signal and can push a failure alert
# to an operator webhook. It is safe to run every minute from cron or a systemd
# timer and safe to run concurrently: it only reads the running stack, writes
# nothing into it, and debounces alert delivery through a small state file.
#
#   scripts/monitor.sh
#
# Signals emitted, in order:
#   containers         every required service container is running
#   api_live           GET /health/live answered ok
#   api_ready          GET /health/ready answered ready
#   database_ready     PostgreSQL accepts connections
#   database_runtime   the runtime role answered a real query
#   redis_auth         Redis answered an authenticated PING
#   rabbitmq_up        rabbitmqctl status is ok
#   queue_present      the expected queue exists
#   queue_backlog      queue depth vs AUSTRO_QUEUE_BACKLOG_THRESHOLD
#   worker_attached    the worker started and attached to the queue
#   worker_errors      error-level lines in the worker log window
#   backup_fresh       newest local backup age vs AUSTRO_BACKUP_MAX_AGE_HOURS
#   proxy_https        the reverse proxy served a real HTTPS request
#   alert_delivered    webhook outcome, emitted only when a signal failed
#   summary            pass/fail counts and the process verdict
#
# Environment (every variable carries the same default the scripts and the
# compose file use, so an operator can never read one behaviour and get
# another):
#   AUSTRO_ENV_FILE                    default .env.production
#   AUSTRO_PROXY_SERVICE               default reverse-proxy
#   AUSTRO_MONITOR_SKIP_PROXY          1 skips the proxy HTTPS signal
#   AUSTRO_QUEUE_BACKLOG_THRESHOLD     default 100 messages
#   AUSTRO_MONITOR_WORKER_ERROR_LIMIT  default 0 = report, never fail
#   AUSTRO_BACKUP_MAX_AGE_HOURS        default 24 hours
#   AUSTRO_ALERT_WEBHOOK_URL           POST target for failure alerts
#   AUSTRO_ALERT_MIN_INTERVAL_SECONDS  default 3600 (debounce)
#   AUSTRO_ALERT_STATE_DIR             default /tmp/austro-monitor
#
# Exit status: 0 when no signal FAILED (a signal that could not be evaluated
# is reported as info, never as pass). 1 when at least one signal FAILED.

set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_FILE="docker-compose.production.yml"
ENV_FILE="${AUSTRO_ENV_FILE:-.env.production}"
PROXY_SERVICE="${AUSTRO_PROXY_SERVICE:-reverse-proxy}"
SKIP_PROXY="${AUSTRO_MONITOR_SKIP_PROXY:-0}"

QUEUE_BACKLOG_THRESHOLD="${AUSTRO_QUEUE_BACKLOG_THRESHOLD:-100}"
WORKER_ERROR_LIMIT="${AUSTRO_MONITOR_WORKER_ERROR_LIMIT:-0}"
BACKUP_MAX_AGE_HOURS="${AUSTRO_BACKUP_MAX_AGE_HOURS:-24}"
ALERT_WEBHOOK="${AUSTRO_ALERT_WEBHOOK_URL:-}"
ALERT_MIN_INTERVAL="${AUSTRO_ALERT_MIN_INTERVAL_SECONDS:-3600}"
ALERT_STATE_DIR="${AUSTRO_ALERT_STATE_DIR:-/tmp/austro-monitor}"

PASS_COUNT=0
FAIL_COUNT=0
FAILED_SIGNALS=""

# jstr escapes the two characters that matter inside a JSON string literal.
jstr() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
emit() { printf '{"signal":"%s","status":"%s","detail":"%s"}\n' "$1" "$2" "$(jstr "$3")"; }
ok()   { emit "$1" pass "$2"; PASS_COUNT=$((PASS_COUNT + 1)); }
bad()  { emit "$1" fail "$2" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); FAILED_SIGNALS="$FAILED_SIGNALS $1"; }
note() { emit "$1" info "$2"; }
die()  { printf '[monitor] ERROR: %s\n' "$*" >&2; exit 1; }

compose() { docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
command -v docker >/dev/null 2>&1 || die "docker is not installed or not on PATH"
docker info >/dev/null 2>&1 || die "cannot talk to the docker daemon"
[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE (needed for the database, cache and broker credentials)"

set -a
# shellcheck source=/dev/null
. "$ENV_FILE"
set +a

DB_OWNER="${AUSTRO_POSTGRES_USER:-austro}"
DB_NAME="${AUSTRO_POSTGRES_DB:-austro}"
DB_RUNTIME_USER="${AUSTRO_POSTGRES_RUNTIME_USER:-austro_app}"
QUEUE_NAME="${AUSTRO_RABBITMQ_QUEUE:-austro.events}"

# No safe default exists for these two: a wrong password is not a value this
# script can invent, and an empty one would fail later with a confusing error.
for required_secret in AUSTRO_POSTGRES_RUNTIME_PASSWORD AUSTRO_REDIS_PASSWORD; do
    [[ -n "${!required_secret:-}" ]] \
        || die "$ENV_FILE does not define $required_secret (needed to authenticate as the runtime role and to Redis)"
done
DB_RUNTIME_PASSWORD="${AUSTRO_POSTGRES_RUNTIME_PASSWORD:-}"
REDIS_PASSWORD="${AUSTRO_REDIS_PASSWORD:-}"

now_epoch="$(date +%s)"

printf '=== AUSTRO OS monitor ===\n'

# ---------------------------------------------------------------------------
# 1. Containers — every required service must be running.
# ---------------------------------------------------------------------------
REQUIRED_SERVICES=(postgres redis rabbitmq api worker)
if [[ "$SKIP_PROXY" != "1" ]]; then
    REQUIRED_SERVICES+=("$PROXY_SERVICE")
fi

NOT_RUNNING=""
for service in "${REQUIRED_SERVICES[@]}"; do
    container_id="$(compose ps -q "$service" 2>/dev/null || true)"
    if [[ -z "$container_id" ]]; then
        NOT_RUNNING="$NOT_RUNNING $service"
        continue
    fi
    state="$(docker inspect --format '{{.State.Status}}' "$container_id" 2>/dev/null || echo unknown)"
    if [[ "$state" != "running" ]]; then
        NOT_RUNNING="$NOT_RUNNING $service"
    fi
done
if [[ -n "$NOT_RUNNING" ]]; then
    bad containers "expected services not running:$NOT_RUNNING"
else
    ok containers "every required service container is running"
fi

# ---------------------------------------------------------------------------
# 2. API — liveness and readiness.
# ---------------------------------------------------------------------------
api_response="$(compose exec -T api wget -qO- http://127.0.0.1:8080/health/live 2>/dev/null || true)"
if [[ "$api_response" == *'"status":"ok"'* ]]; then
    ok api_live "GET /health/live answered ok"
else
    bad api_live "GET /health/live did not answer ok (got: ${api_response:-<empty>})"
fi

ready_response="$(compose exec -T api wget -qO- http://127.0.0.1:8080/health/ready 2>/dev/null || true)"
if [[ "$ready_response" == *'"ready":true'* ]]; then
    ok api_ready "GET /health/ready answered ready"
else
    bad api_ready "GET /health/ready did not answer ready (got: ${ready_response:-<empty>})"
fi

# ---------------------------------------------------------------------------
# 3. PostgreSQL — accepting connections, then a query as the runtime role.
# ---------------------------------------------------------------------------
if compose exec -T postgres pg_isready -U "$DB_OWNER" -d "$DB_NAME" -h 127.0.0.1 >/dev/null 2>&1; then
    ok database_ready "PostgreSQL is accepting connections"
else
    bad database_ready "PostgreSQL is not accepting connections"
fi

runtime_query="$(compose exec -T -e PGPASSWORD="$DB_RUNTIME_PASSWORD" postgres \
    psql -U "$DB_RUNTIME_USER" -d "$DB_NAME" -h 127.0.0.1 -tAc 'SELECT 1' 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$runtime_query" == "1" ]]; then
    ok database_runtime "runtime role ($DB_RUNTIME_USER) answered a real query"
else
    bad database_runtime "runtime role ($DB_RUNTIME_USER) did not answer a query"
fi

# ---------------------------------------------------------------------------
# 4. Redis — authenticated ping.
# ---------------------------------------------------------------------------
redis_ping="$(compose exec -T -e REDISCLI_AUTH="$REDIS_PASSWORD" redis \
    redis-cli --no-auth-warning ping 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$redis_ping" == "PONG" ]]; then
    ok redis_auth "Redis answered an authenticated PING"
else
    bad redis_auth "Redis did not answer an authenticated PING (got: ${redis_ping:-<empty>})"
fi

# ---------------------------------------------------------------------------
# 5. RabbitMQ and the expected queue, including backlog depth.
# ---------------------------------------------------------------------------
if compose exec -T rabbitmq rabbitmqctl status >/dev/null 2>&1; then
    ok rabbitmq_up "rabbitmqctl status is ok"
else
    bad rabbitmq_up "rabbitmqctl status is not ok"
fi

# The stream is consumed fully before grep looks at it, so an absent queue or a
# radio-silent broker cannot SIGPIPE the producer into a synthetic failure.
queue_line="$(compose exec -T rabbitmq rabbitmqctl list_queues name messages 2>/dev/null \
    | grep -E "^[[:space:]]*${QUEUE_NAME}([[:space:]]|$)" || true)"
if [[ -z "$queue_line" ]]; then
    bad queue_present "expected queue is missing: $QUEUE_NAME"
    bad queue_backlog "cannot measure backlog: queue $QUEUE_NAME is missing"
else
    ok queue_present "expected queue exists: $QUEUE_NAME"
    backlog_value="$(printf '%s\n' "$queue_line" | awk '{print $NF}' | tr -d '\r ' || true)"
    if [[ "$backlog_value" =~ ^[0-9]+$ ]]; then
        if (( backlog_value > QUEUE_BACKLOG_THRESHOLD )); then
            bad queue_backlog "queue depth $backlog_value exceeds threshold $QUEUE_BACKLOG_THRESHOLD"
        else
            ok queue_backlog "queue depth $backlog_value is within threshold $QUEUE_BACKLOG_THRESHOLD"
        fi
    else
        bad queue_backlog "could not read a numeric queue depth (raw: ${queue_line:-<empty>})"
    fi
fi

# ---------------------------------------------------------------------------
# 6. Worker — attached to the queue, and a bounded error window.
# ---------------------------------------------------------------------------
worker_logs="$(compose logs --no-log-prefix --tail=2000 worker 2>/dev/null || true)"

if grep -q '"message":"worker-started"' <<<"$worker_logs"; then
    ok worker_attached "worker reported worker-started"
else
    bad worker_attached "worker never reported worker-started"
fi

# grep -c exits 1 on a zero count but still prints 0; the || true keeps the
# command substitution from failing, and the numeric guard keeps the signal
# honest.
error_count="$(grep -c '"level":"error"' <<<"$worker_logs" || true)"
if [[ "$error_count" =~ ^[0-9]+$ ]]; then
    if (( WORKER_ERROR_LIMIT > 0 )) && (( error_count > WORKER_ERROR_LIMIT )); then
        bad worker_errors "$error_count error-level line(s) in the worker log window exceed limit $WORKER_ERROR_LIMIT"
    else
        ok worker_errors "worker log window shows $error_count error-level line(s)"
    fi
else
    note worker_errors "could not read the worker log window"
fi

# ---------------------------------------------------------------------------
# 7. Backup freshness — the newest local backup must be recent enough.
# ---------------------------------------------------------------------------
if [[ -d backups ]]; then
    newest="$(ls -t backups/austro-postgres-*.sql.gz 2>/dev/null | tail -1 || true)"
else
    newest=""
fi
if [[ -z "$newest" ]]; then
    bad backup_fresh "no backup artifact found in backups/"
else
    newest_mtime="$(stat -c '%Y' "$newest" 2>/dev/null || echo 0)"
    age_hours=$(( (now_epoch - newest_mtime) / 3600 ))
    if (( age_hours <= BACKUP_MAX_AGE_HOURS )); then
        ok backup_fresh "newest backup $newest is ${age_hours}h old (limit ${BACKUP_MAX_AGE_HOURS}h)"
    else
        bad backup_fresh "newest backup $newest is ${age_hours}h old (limit ${BACKUP_MAX_AGE_HOURS}h)"
    fi
fi

# ---------------------------------------------------------------------------
# 8. Reverse proxy — a real HTTPS request, when not skipped.
# ---------------------------------------------------------------------------
if [[ "$SKIP_PROXY" == "1" ]]; then
    note proxy_https "proxy signal skipped (AUSTRO_MONITOR_SKIP_PROXY=1)"
elif ! command -v curl >/dev/null 2>&1; then
    note proxy_https "proxy signal not evaluated: curl is not available on this host"
else
    proxy_host="${AUSTRO_PUBLIC_HOSTNAME:-localhost}"
    https_code="$(curl -s -o /dev/null -w '%{http_code}' -k --max-time 15 --noproxy '*' \
        --resolve "${proxy_host}:443:127.0.0.1" "https://${proxy_host}/health/live" 2>/dev/null || true)"
    if [[ "$https_code" == "200" ]]; then
        ok proxy_https "reverse proxy served a real HTTPS request (HTTP 200 on /health/live)"
    else
        bad proxy_https "reverse proxy did not serve /health/live over HTTPS (HTTP ${https_code:-no response})"
    fi
fi

# ---------------------------------------------------------------------------
# 9. Alert delivery — only after a failure, debounced, never an extra failure.
# ---------------------------------------------------------------------------
if (( FAIL_COUNT > 0 )) && [[ -n "$ALERT_WEBHOOK" ]] && command -v curl >/dev/null 2>&1; then
    mkdir -p "$ALERT_STATE_DIR"
    last_alert_epoch="$(cat "$ALERT_STATE_DIR/last-alert" 2>/dev/null || echo 0)"
    if [[ "$last_alert_epoch" =~ ^[0-9]+$ ]] && (( now_epoch - last_alert_epoch < ALERT_MIN_INTERVAL )); then
        note alert_delivered "alert suppressed by the debounce window (last alert at epoch $last_alert_epoch)"
    else
        payload="{\"system\":\"austro-os\",\"status\":\"degraded\",\"failed\":\"${FAILED_SIGNALS}\",\"timestamp\":${now_epoch}}"
        if curl -s --max-time 15 --noproxy '*' -H 'Content-Type: application/json' \
            -d "$payload" "$ALERT_WEBHOOK" >/dev/null 2>&1; then
            printf '%s\n' "$now_epoch" > "$ALERT_STATE_DIR/last-alert"
            note alert_delivered "alert accepted by the webhook"
        else
            note alert_delivered "alert could not be delivered to the webhook (will retry after the debounce window)"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
if (( FAIL_COUNT > 0 )); then
    printf '{"signal":"summary","status":"fail","detail":"%d passed, %d failed: %s"}\n' \
        "$PASS_COUNT" "$FAIL_COUNT" "$(jstr "$FAILED_SIGNALS")"
    printf '\nMONITOR: DEGRADED (%s)\n' "${FAILED_SIGNALS# }" >&2
    exit 1
fi
printf '{"signal":"summary","status":"pass","detail":"%d passed, %d failed"}\n' "$PASS_COUNT" "$FAIL_COUNT"
printf '\nMONITOR: NOMINAL\n'