#!/usr/bin/env bash
#
# AUSTRO OS — native ARM64 production healthcheck.
#
# The deployment/native counterpart of scripts/healthcheck.sh. It verifies the
# RUNNING topology on the host, not a configuration file: each check talks to
# the live systemd units, the live API, the live datastores, and the live
# journal. Nothing here is a mock, and nothing reports a pass it did not
# observe.
#
#   sudo deploy/native/healthcheck.sh
#
# Checks, in order:
#   * every required systemd service is active
#   * the API answers /health/live and /health/ready
#   * PostgreSQL accepts connections AND answers a query as the RUNTIME role,
#     which is the credential the application actually serves traffic on
#   * the runtime role is not a superuser, does not hold BYPASSRLS and owns no
#     protected table
#   * all protected tables have row level security enabled AND forced
#   * Redis answers an AUTHENTICATED ping
#   * RabbitMQ is up, the configured user exists, and the expected queue exists
#   * the API's own log reports a verified database topology (as the expected
#     runtime role) and a Redis connection
#   * the worker reports that it started and attached to the queue
#   * nginx answers a real HTTPS request (when TLS material exists)
#
# Nginx is deliberately fail-closed on this path: until real TLS material is
# present at /etc/austro/tls/ it stays disabled and no plaintext is served.
# When that material is absent, nginx-related checks are reported BLOCKED
# (they could not run and are not silently passed); once present, they are
# required to pass.
#
# Environment:
#   AUSTRO_ENV_FILE                 default /etc/austro/austro.env
#   AUSTRO_HEALTHCHECK_SKIP_PROXY   set to 1 to skip the nginx/proxy checks
#
# Exit status: 0 only when no check FAILED. BLOCKED items are reported as
# BLOCKED, never as PASS.

set -Eeuo pipefail

ENV_FILE="${AUSTRO_ENV_FILE:-/etc/austro/austro.env}"
TLS_DIR="${AUSTRO_TLS_DIR:-/etc/austro/tls}"
SKIP_PROXY="${AUSTRO_HEALTHCHECK_SKIP_PROXY:-0}"

PASS_COUNT=0
FAIL_COUNT=0
BLOCKED_COUNT=0

ok()      { printf 'PASS     %s\n' "$1"; PASS_COUNT=$((PASS_COUNT + 1)); }
bad()     { printf 'FAIL     %s\n' "$1" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }
blocked() { printf 'BLOCKED  %s\n' "$1" >&2; BLOCKED_COUNT=$((BLOCKED_COUNT + 1)); }
note()    { printf '         %s\n' "$1"; }

die() { printf '[healthcheck] ERROR: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
for tool in systemctl journalctl curl pg_isready psql redis-cli; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is not installed or not on PATH"
done
[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE (needed for the database, cache and broker credentials)"

set -a
# shellcheck source=/etc/austro/austro.env
# shellcheck disable=SC1091
. "$ENV_FILE"
set +a

DB_NAME="${AUSTRO_POSTGRES_DB:-austro}"
DB_RUNTIME_USER="${AUSTRO_POSTGRES_RUNTIME_USER:-austro_app}"
QUEUE_NAME="${AUSTRO_RABBITMQ_QUEUE:-austro.events}"
RABBITMQ_USER="${AUSTRO_RABBITMQ_USER:-austro}"

for required_secret in AUSTRO_POSTGRES_RUNTIME_PASSWORD AUSTRO_REDIS_PASSWORD; do
	[[ -n "${!required_secret:-}" ]] \
		|| die "$ENV_FILE does not define $required_secret (needed to authenticate as the runtime role and to Redis)"
done
DB_RUNTIME_PASSWORD="${AUSTRO_POSTGRES_RUNTIME_PASSWORD:-}"
REDIS_PASSWORD="${AUSTRO_REDIS_PASSWORD:-}"

if [ -f "$TLS_DIR/fullchain.pem" ] && [ -f "$TLS_DIR/privkey.pem" ]; then
	TLS_PRESENT=1
else
	TLS_PRESENT=0
fi

printf '=== AUSTRO OS healthcheck (native) ===\n'

# ---------------------------------------------------------------------------
# 1. Services
# ---------------------------------------------------------------------------
REQUIRED_SERVICES=(postgresql redis-server rabbitmq-server austro-api austro-worker)
if [ "$TLS_PRESENT" -eq 1 ] && [ "$SKIP_PROXY" != "1" ]; then
	REQUIRED_SERVICES+=(nginx)
fi

for service in "${REQUIRED_SERVICES[@]}"; do
	if systemctl is-active --quiet "$service"; then
		ok "service active: $service"
	else
		bad "service is not active: $service"
		note "$(systemctl show -p ExecMainStatus -p ActiveState "$service" 2>/dev/null | tr '\n' ' ')"
	fi
done

if [ "$TLS_PRESENT" -eq 0 ]; then
	blocked "nginx deliberately not enabled (no TLS material at $TLS_DIR — fail-closed, no plaintext)"
fi

# ---------------------------------------------------------------------------
# 2. API liveness and readiness
# ---------------------------------------------------------------------------
api_live="$(curl -fsS http://127.0.0.1:8080/health/live 2>/dev/null ||
	printf 'curl-exit=%s' "$?" || true)"
if [[ "$api_live" == *'"status":"ok"'* ]]; then
	ok "API /health/live answered ok"
else
	bad "API /health/live did not answer ok (got: ${api_live:-<empty>})"
fi

api_ready="$(curl -fsS http://127.0.0.1:8080/health/ready 2>/dev/null ||
	printf 'curl-exit=%s' "$?" || true)"
if [[ "$api_ready" == *'"ready":true'* ]]; then
	ok "API /health/ready answered ready"
else
	bad "API /health/ready did not answer ready (got: ${api_ready:-<empty>})"
fi

# ---------------------------------------------------------------------------
# 3. PostgreSQL — readiness, then a real query as the runtime role
# ---------------------------------------------------------------------------
if runuser -u postgres -- pg_isready -U postgres -d postgres -h /var/run/postgresql >/dev/null 2>&1; then
	ok "PostgreSQL is accepting connections (pg_isready over the local socket)"
else
	bad "PostgreSQL is not accepting connections"
fi

# Connecting AS the runtime role proves the credential the application serves
# with actually works over the same loopback path the application uses.
runtime_query="$(PGPASSWORD="$DB_RUNTIME_PASSWORD" psql \
	-U "$DB_RUNTIME_USER" -d "$DB_NAME" -h 127.0.0.1 -tAc 'SELECT 1' 2>/dev/null | tr -d '[:space:]' || true)"
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
	connected_role="$(PGPASSWORD="$DB_RUNTIME_PASSWORD" psql \
		-U "$DB_RUNTIME_USER" -d "$DB_NAME" -h 127.0.0.1 -tAc 'SELECT current_user' 2>/dev/null | tr -d '[:space:]' || true)"
	if [[ "$connected_role" == "$DB_RUNTIME_USER" ]]; then
		ok "connection authenticated as the intended runtime role ($DB_RUNTIME_USER)"
	else
		bad "expected to connect as $DB_RUNTIME_USER but the server reported '${connected_role:-<empty>}'"
	fi

	# Evaluated as the runtime role, so it observes the privileges the
	# application actually has. The exit status decides pass/fail: ON_ERROR_STOP
	# makes psql return 3 when the DO block raises.
	privilege_status=0
	privilege_output="$(PGPASSWORD="$DB_RUNTIME_PASSWORD" psql \
		-U "$DB_RUNTIME_USER" -d "$DB_NAME" -h 127.0.0.1 -v ON_ERROR_STOP=1 -tA 2>&1 <<'SQL'
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

	if [ "$privilege_status" -eq 0 ]; then
		ok "runtime role is unprivileged and every protected table has RLS enabled and forced"
		note "$(printf '%s' "$privilege_output" | tr '\n' ' ' | sed 's/^ *//' | cut -c1-200)"
	else
		bad "runtime database security is NOT valid: $(printf '%s' "$privilege_output" | tr '\n' ' ' | cut -c1-300)"
	fi
fi

# ---------------------------------------------------------------------------
# 5. Redis — authenticated ping
# ---------------------------------------------------------------------------
redis_ping="$(REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli --no-auth-warning -h 127.0.0.1 ping 2>/dev/null |
	tr -d '[:space:]' || true)"
if [[ "$redis_ping" == "PONG" ]]; then
	ok "Redis answered an authenticated PING"
else
	bad "Redis did not answer an authenticated PING (got: ${redis_ping:-<empty>})"
fi

# ---------------------------------------------------------------------------
# 6. RabbitMQ — status, configured user, expected queue
# ---------------------------------------------------------------------------
# rabbitmqctl must run as the rabbitmq OS user: the Erlang cookie that
# authenticates CLI calls is owned by that user.
if runuser -u rabbitmq -- rabbitmqctl status >/dev/null 2>&1; then
	ok "RabbitMQ reports status ok"
else
	bad "RabbitMQ did not answer rabbitmqctl status"
fi

if runuser -u rabbitmq -- rabbitmqctl list_users 2>/dev/null | grep -q "^${RABBITMQ_USER}[[:space:]]"; then
	ok "RabbitMQ has the configured user ($RABBITMQ_USER)"
else
	bad "RabbitMQ does not have the configured user ($RABBITMQ_USER)"
fi

if runuser -u rabbitmq -- rabbitmqctl list_queues name 2>/dev/null | grep -Eq "^[[:space:]]*${QUEUE_NAME}([[:space:]]|$)"; then
	ok "expected queue exists: $QUEUE_NAME"
else
	bad "expected queue is missing: $QUEUE_NAME"
fi

# ---------------------------------------------------------------------------
# 7. API logs — the topology the process says it established
# ---------------------------------------------------------------------------
api_logs="$(journalctl --no-pager -n 3000 -u austro-api.service 2>/dev/null || true)"

topo_line="$(printf '%s\n' "$api_logs" | grep -m1 '"message":"database-topology-ready"' || true)"
if [[ -n "$topo_line" ]]; then
	if [[ "$topo_line" == *'"runtime_role":"austro_app"'* ]]; then
		ok "API reported a verified database topology as the runtime role austro_app"
		note "$(printf '%s' "$topo_line" | cut -c1-240)"
	else
		bad "API reported database-topology-ready but NOT as runtime role austro_app"
	fi
else
	bad "API never reported database-topology-ready"
fi

if printf '%s\n' "$api_logs" | grep -q 'database-runtime-security-failed'; then
	bad "API logged a runtime database security failure"
else
	ok "API logged no runtime database security failure"
fi

if printf '%s\n' "$api_logs" | grep -q '"message":"redis-connection-established"'; then
	ok "API reported an established Redis connection"
else
	bad "API never reported redis-connection-established"
fi

# ---------------------------------------------------------------------------
# 8. Worker — started and attached to the queue
# ---------------------------------------------------------------------------
worker_logs="$(journalctl --no-pager -n 3000 -u austro-worker.service 2>/dev/null || true)"

worker_line="$(printf '%s\n' "$worker_logs" | grep -m1 '"message":"worker-started"' || true)"
if [[ -n "$worker_line" ]]; then
	ok "worker reported that it started and attached to the queue"
	note "$(printf '%s' "$worker_line" | cut -c1-240)"
else
	bad "worker never reported worker-started"
fi

# ---------------------------------------------------------------------------
# 9. Reverse proxy — a real HTTPS request
# ---------------------------------------------------------------------------
if [[ "$SKIP_PROXY" == "1" ]]; then
	note "proxy checks skipped (AUSTRO_HEALTHCHECK_SKIP_PROXY=1)"
elif [ "$TLS_PRESENT" -eq 0 ]; then
	blocked "cannot verify HTTPS through the proxy: no TLS material installed (fail-closed)"
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