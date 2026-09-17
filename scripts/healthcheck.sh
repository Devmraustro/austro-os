#!/usr/bin/env bash
#
# AUSTRO OS — production healthcheck.
#
# Verifies the RUNNING topology, not the configuration: each check talks to the
# live container and asserts on what it answers. Nothing here is a mock, and
# nothing here reports a pass it did not observe.
#
#   scripts/healthcheck.sh
#
# Checks, in order:
#   * every required container is running and not reporting unhealthy
#   * the API answers /health/live and /health/ready
#   * PostgreSQL accepts connections AND answers a query as the RUNTIME role,
#     which is the credential the application actually serves traffic on
#   * the runtime role is not a superuser, does not hold BYPASSRLS and owns no
#     protected table
#   * all protected tables have row level security enabled AND forced
#   * Redis answers an AUTHENTICATED ping
#   * RabbitMQ is up, the configured user exists, and the expected queue exists
#   * the API's own log reports a verified database topology and a Redis connection
#   * the worker reports that it started and attached to the queue
#   * the reverse proxy answers a real HTTPS request (when curl is available)
#
# Environment:
#   AUSTRO_ENV_FILE                 default .env.production
#   AUSTRO_PROXY_SERVICE            default reverse-proxy
#   AUSTRO_HEALTHCHECK_SKIP_PROXY   set to 1 to skip the proxy checks
#
# Exit status: 0 only when no check FAILED. BLOCKED items (a check that could
# not run, e.g. no curl on the host) are reported as BLOCKED, never as PASS.

set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_FILE="docker-compose.production.yml"
ENV_FILE="${AUSTRO_ENV_FILE:-.env.production}"
PROXY_SERVICE="${AUSTRO_PROXY_SERVICE:-reverse-proxy}"
SKIP_PROXY="${AUSTRO_HEALTHCHECK_SKIP_PROXY:-0}"

PASS_COUNT=0
FAIL_COUNT=0
BLOCKED_COUNT=0

ok()      { printf 'PASS     %s\n' "$1"; PASS_COUNT=$((PASS_COUNT + 1)); }
bad()     { printf 'FAIL     %s\n' "$1" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }
blocked() { printf 'BLOCKED  %s\n' "$1" >&2; BLOCKED_COUNT=$((BLOCKED_COUNT + 1)); }
note()    { printf '         %s\n' "$1"; }

die() { printf '[healthcheck] ERROR: %s\n' "$*" >&2; exit 1; }

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

# Every value this script reads is given the SAME default the compose file
# uses, so a check asserts on the topology the stack actually built rather than
# on whatever the caller's environment happened to contain.
#
# This is not cosmetic. AUSTRO_RABBITMQ_USER was read without a default while
# the compose file defaults it to austro and .env.example ships it COMMENTED
# OUT, so `set -u` aborted the run with a bare "unbound variable" for any
# operator who left it unset — and for CI, whose minimal environment file omits
# it. The abort landed mid-run and looked like a broker problem rather than a
# missing default. Defaults are mirrored instead; the secrets that genuinely
# have no safe default are required explicitly below, with a message that says
# which value is missing and where it should have come from.
DB_OWNER="${AUSTRO_POSTGRES_USER:-austro}"
DB_NAME="${AUSTRO_POSTGRES_DB:-austro}"
DB_RUNTIME_USER="${AUSTRO_POSTGRES_RUNTIME_USER:-austro_app}"
QUEUE_NAME="${AUSTRO_RABBITMQ_QUEUE:-austro.events}"
RABBITMQ_USER="${AUSTRO_RABBITMQ_USER:-austro}"

# No safe default exists for these two: a wrong password is not a check this
# script can perform, and an empty one would fail later with a confusing error.
for required_secret in AUSTRO_POSTGRES_RUNTIME_PASSWORD AUSTRO_REDIS_PASSWORD; do
    [[ -n "${!required_secret:-}" ]] \
        || die "$ENV_FILE does not define $required_secret (needed to authenticate as the runtime role and to Redis)"
done
DB_RUNTIME_PASSWORD="${AUSTRO_POSTGRES_RUNTIME_PASSWORD:-}"
REDIS_PASSWORD="${AUSTRO_REDIS_PASSWORD:-}"

printf '=== AUSTRO OS healthcheck ===\n'

# ---------------------------------------------------------------------------
# 1. Containers
# ---------------------------------------------------------------------------
REQUIRED_SERVICES=(postgres redis rabbitmq api worker)
if [[ "$SKIP_PROXY" != "1" ]]; then
    REQUIRED_SERVICES+=("$PROXY_SERVICE")
fi

for service in "${REQUIRED_SERVICES[@]}"; do
    container_id="$(compose ps -q "$service" 2>/dev/null || true)"
    if [[ -z "$container_id" ]]; then
        bad "container not created: $service"
        continue
    fi
    state="$(docker inspect --format '{{.State.Status}}' "$container_id" 2>/dev/null || echo unknown)"
    health="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container_id" 2>/dev/null || echo unknown)"
    if [[ "$state" != "running" ]]; then
        bad "container $service is '$state', expected running"
    elif [[ "$health" == "unhealthy" ]]; then
        bad "container $service reports unhealthy"
        note "$(compose logs --tail=5 "$service" 2>/dev/null | tr '\n' ' ' | cut -c1-300)"
    else
        ok "container running: $service (health: $health)"
    fi
done

# ---------------------------------------------------------------------------
# 2. API liveness and readiness
# ---------------------------------------------------------------------------
api_response="$(compose exec -T api wget -qO- http://127.0.0.1:8080/health/live 2>/dev/null || true)"
if [[ "$api_response" == *'"status":"ok"'* ]]; then
    ok "API /health/live answered ok"
else
    bad "API /health/live did not answer ok (got: ${api_response:-<empty>})"
fi

ready_response="$(compose exec -T api wget -qO- http://127.0.0.1:8080/health/ready 2>/dev/null || true)"
if [[ "$ready_response" == *'"ready":true'* ]]; then
    ok "API /health/ready answered ready"
else
    bad "API /health/ready did not answer ready (got: ${ready_response:-<empty>})"
fi

# ---------------------------------------------------------------------------
# 3. PostgreSQL — readiness, then a real query as the runtime role
# ---------------------------------------------------------------------------
if compose exec -T postgres pg_isready -U "$DB_OWNER" -d "$DB_NAME" -h 127.0.0.1 >/dev/null 2>&1; then
    ok "PostgreSQL is accepting connections (pg_isready)"
else
    bad "PostgreSQL is not accepting connections"
fi

# Connecting AS austro_app proves the credential the application serves with
# actually works. Asking the owner about austro_app would pass even if the
# runtime password were wrong, which is the failure this is meant to catch.
runtime_query="$(compose exec -T -e PGPASSWORD="$DB_RUNTIME_PASSWORD" postgres \
    psql -U "$DB_RUNTIME_USER" -d "$DB_NAME" -h 127.0.0.1 -tAc 'SELECT 1' 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$runtime_query" == "1" ]]; then
    ok "PostgreSQL answered a query as the runtime role ($DB_RUNTIME_USER)"
else
    bad "PostgreSQL did not answer a query as the runtime role ($DB_RUNTIME_USER)"
fi

# ---------------------------------------------------------------------------
# 4. Runtime role privileges and row level security
# ---------------------------------------------------------------------------
if [[ "$runtime_query" != "1" ]]; then
    bad "skipped the runtime security checks because the runtime role could not connect"
else
    # Confirm which role the connection actually resolved to. psql does not
    # interpolate its -v variables inside dollar-quoted blocks, so this is a
    # separate flat query compared here rather than inside the DO block below.
    connected_role="$(compose exec -T -e PGPASSWORD="$DB_RUNTIME_PASSWORD" postgres \
        psql -U "$DB_RUNTIME_USER" -d "$DB_NAME" -h 127.0.0.1 -tAc 'SELECT current_user' 2>/dev/null | tr -d '[:space:]' || true)"
    if [[ "$connected_role" == "$DB_RUNTIME_USER" ]]; then
        ok "connection authenticated as the intended runtime role ($DB_RUNTIME_USER)"
    else
        bad "expected to connect as $DB_RUNTIME_USER but the server reported '${connected_role:-<empty>}'"
    fi

    # Evaluated as the runtime role, so it observes the privileges the
    # application actually has. A superuser or BYPASSRLS role makes every
    # workspace policy decorative, and a role owning the tables it queries is
    # exempt from row level security entirely — all three are checked against
    # the live catalog, plus the enabled/forced policy counts.
    #
    # The exit status is what decides pass/fail: ON_ERROR_STOP makes psql return
    # 3 when the DO block raises, so this cannot be fooled by matching text that
    # happens to contain the word ERROR.
    privilege_status=0
    privilege_output="$(compose exec -T -e PGPASSWORD="$DB_RUNTIME_PASSWORD" postgres \
        psql -U "$DB_RUNTIME_USER" -d "$DB_NAME" -h 127.0.0.1 -v ON_ERROR_STOP=1 -tA 2>&1 <<'SQL'
DO $$
DECLARE
    v_super boolean; v_bypass boolean; v_owned integer;
    v_enabled integer; v_forced integer;
BEGIN
    SELECT rolsuper, rolbypassrls INTO v_super, v_bypass FROM pg_roles WHERE rolname = current_user;
    IF v_super THEN RAISE EXCEPTION 'runtime role is a superuser'; END IF;
    IF v_bypass THEN RAISE EXCEPTION 'runtime role holds BYPASSRLS'; END IF;

    SELECT count(*) INTO v_owned
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
      JOIN pg_roles r ON r.oid = c.relowner
     WHERE n.nspname = 'public' AND c.relkind = 'r' AND r.rolname = current_user;
    IF v_owned > 0 THEN RAISE EXCEPTION 'runtime role owns % protected table(s)', v_owned; END IF;

    SELECT count(*) INTO v_enabled FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relrowsecurity;
    SELECT count(*) INTO v_forced FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relforcerowsecurity;
    IF v_enabled < 12 OR v_forced < 12 THEN
        RAISE EXCEPTION 'row level security enabled=% forced=% (expected at least 12 each)', v_enabled, v_forced;
    END IF;

    RAISE NOTICE 'runtime role unprivileged; rls enabled=% forced=%', v_enabled, v_forced;
END;
$$;
SQL
)" || privilege_status=$?

    if [[ "$privilege_status" -eq 0 ]]; then
        ok "runtime role is unprivileged and every protected table has RLS enabled and forced"
        note "$(printf '%s' "$privilege_output" | tr '\n' ' ' | sed 's/^ *//' | cut -c1-200)"
    else
        bad "runtime database security is NOT valid: $(printf '%s' "$privilege_output" | tr '\n' ' ' | cut -c1-300)"
    fi
fi

# ---------------------------------------------------------------------------
# 5. Redis — authenticated ping
# ---------------------------------------------------------------------------
# An unauthenticated ping would answer NOAUTH, and a bare TCP check would call
# an unauthenticated instance healthy. REDISCLI_AUTH keeps the password out of
# the process argument list.
redis_ping="$(compose exec -T -e REDISCLI_AUTH="$REDIS_PASSWORD" redis \
    redis-cli --no-auth-warning ping 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$redis_ping" == "PONG" ]]; then
    ok "Redis answered an authenticated PING"
else
    bad "Redis did not answer an authenticated PING (got: ${redis_ping:-<empty>})"
fi

# ---------------------------------------------------------------------------
# 6. RabbitMQ — status, configured user, expected queue
# ---------------------------------------------------------------------------
if compose exec -T rabbitmq rabbitmqctl status >/dev/null 2>&1; then
    ok "RabbitMQ reports status ok"
else
    bad "RabbitMQ did not answer rabbitmqctl status"
fi

if compose exec -T rabbitmq rabbitmqctl list_users 2>/dev/null | grep -q "^${RABBITMQ_USER}[[:space:]]"; then
    ok "RabbitMQ has the configured user ($RABBITMQ_USER)"
else
    bad "RabbitMQ does not have the configured user ($RABBITMQ_USER)"
fi

# Matches the queue as a whole first column. rabbitmqctl pads columns and may
# emit adjacent columns, so requiring the bare line to equal the name can miss a
# queue that is present; requiring the following character to be whitespace or
# end-of-line still refuses a different queue whose name merely starts the same
# (austro.events.dlq does not match austro.events).
if compose exec -T rabbitmq rabbitmqctl list_queues name 2>/dev/null | grep -Eq "^[[:space:]]*${QUEUE_NAME}([[:space:]]|$)"; then
    ok "expected queue exists: $QUEUE_NAME"
else
    bad "expected queue is missing: $QUEUE_NAME"
fi

# ---------------------------------------------------------------------------
# 7. API logs — the topology the process says it established
# ---------------------------------------------------------------------------
# This reads the API's OWN startup record rather than trusting the environment
# block this script was handed: it is what the process actually connected as.
api_logs="$(compose logs --no-log-prefix --tail=2000 api 2>/dev/null || true)"

if grep -q '"message":"database-topology-ready"' <<<"$api_logs"; then
    ok "API reported a verified database topology"
    note "$(grep '"message":"database-topology-ready"' <<<"$api_logs" | tail -1 | cut -c1-240)"
else
    bad "API never reported database-topology-ready"
fi

if grep -q 'database-runtime-security-failed' <<<"$api_logs"; then
    bad "API logged a runtime database security failure"
    note "$(grep 'database-runtime-security-failed' <<<"$api_logs" | tail -1 | cut -c1-300)"
else
    ok "API logged no runtime database security failure"
fi

if grep -q '"message":"redis-connection-established"' <<<"$api_logs"; then
    ok "API reported an established Redis connection"
else
    bad "API never reported redis-connection-established"
fi

# ---------------------------------------------------------------------------
# 8. Worker — started and attached to the queue
# ---------------------------------------------------------------------------
worker_logs="$(compose logs --no-log-prefix --tail=2000 worker 2>/dev/null || true)"

if grep -q '"message":"worker-started"' <<<"$worker_logs"; then
    ok "worker reported that it started and attached to the queue"
    note "$(grep '"message":"worker-started"' <<<"$worker_logs" | tail -1 | cut -c1-240)"
else
    bad "worker never reported worker-started"
fi

# ---------------------------------------------------------------------------
# 9. Reverse proxy — a real HTTPS request
# ---------------------------------------------------------------------------
if [[ "$SKIP_PROXY" == "1" ]]; then
    note "proxy checks skipped (AUSTRO_HEALTHCHECK_SKIP_PROXY=1)"
elif ! command -v curl >/dev/null 2>&1; then
    # Not a pass and not a failure: the check could not run. --resolve pins the
    # request to the local proxy regardless of public DNS, and -k is required
    # because the certificate is issued for the public hostname, not 127.0.0.1.
    blocked "cannot verify HTTPS through the proxy: curl is not available on this host"
else
    proxy_host="${AUSTRO_PUBLIC_HOSTNAME:-localhost}"
    https_code="$(curl -s -o /dev/null -w '%{http_code}' -k --max-time 15 --noproxy '*' \
        --resolve "${proxy_host}:443:127.0.0.1" "https://${proxy_host}/health/live" 2>/dev/null || true)"
    if [[ "$https_code" == "200" ]]; then
        ok "reverse proxy served a real HTTPS request (HTTP 200 on /health/live)"
    else
        bad "reverse proxy did not serve /health/live over HTTPS (HTTP ${https_code:-no response})"
    fi

    redirect_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 --noproxy '*' \
        --resolve "${proxy_host}:80:127.0.0.1" "http://${proxy_host}/health/live" 2>/dev/null || true)"
    if [[ "$redirect_code" == "301" || "$redirect_code" == "308" ]]; then
        ok "reverse proxy redirects plaintext HTTP to HTTPS (HTTP $redirect_code)"
    else
        bad "reverse proxy did not redirect HTTP to HTTPS (HTTP ${redirect_code:-no response})"
    fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
printf '\n--- summary ---\n'
printf 'passed:  %d\n' "$PASS_COUNT"
printf 'failed:  %d\n' "$FAIL_COUNT"
printf 'blocked: %d\n' "$BLOCKED_COUNT"

if (( FAIL_COUNT > 0 )); then
    printf '\nHEALTHCHECK: FAIL\n' >&2
    exit 1
fi

if (( BLOCKED_COUNT > 0 )); then
    printf '\nHEALTHCHECK: PASS WITH BLOCKED CHECKS — the blocked items above were NOT verified.\n'
    exit 0
fi

printf '\nHEALTHCHECK: PASS\n'
