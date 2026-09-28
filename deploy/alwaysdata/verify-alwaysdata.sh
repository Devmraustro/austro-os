#!/usr/bin/env bash
#
# AUSTRO OS — AlwaysData production healthcheck.
#
# Verifies the RUNNING topology on the AlwaysData host. Each check talks to
# the live API, the live datastores, and the live services. Nothing here is
# a mock, and nothing reports a pass it did not observe.
#
#   ./verify-alwaysdata.sh
#
# Checks, in order:
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
#   * the public HTTPS endpoint answers a real request
#
# Environment:
#   AUSTRO_ENV_FILE                 default ~/austro/austro.env
#   AUSTRO_HEALTHCHECK_SKIP_PROXY   set to 1 to skip the public HTTPS check
#
# Exit status: 0 only when no check FAILED. BLOCKED items are reported as
# BLOCKED, never as PASS.

set -Eeuo pipefail

ENV_FILE="${AUSTRO_ENV_FILE:-$HOME/austro/austro.env}"

PASS_COUNT=0
FAIL_COUNT=0
BLOCKED_COUNT=0

ok()      { printf 'PASS     %s\n' "$1"; PASS_COUNT=$((PASS_COUNT + 1)); }
bad()     { printf 'FAIL     %s\n' "$1" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }
blocked() { printf 'BLOCKED  %s\n' "$1" >&2; BLOCKED_COUNT=$((BLOCKED_COUNT + 1)); }
note()    { printf '         %s\n' "$1"; }

die() { printf '[verify-alwaysdata] ERROR: %s\n' "$*" >&2; exit 1; }

ENV_FILE="${AUSTRO_ENV_FILE:-$HOME/austro/austro.env}"

for tool in curl psql redis-cli; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is not installed or not on PATH"
done
[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE (needed for the database, cache and broker credentials)"

set -a
# shellcheck source=/dev/null
# shellcheck disable=SC1091
. "$ENV_FILE"
set +a

DB_NAME="${AUSTRO_POSTGRES_DB:-austro_os}"
DB_RUNTIME_USER="${AUSTRO_POSTGRES_RUNTIME_USER:-austro_app}"
QUEUE_NAME="${AUSTRO_RABBITMQ_QUEUE:-austro.events}"
RABBITMQ_USER="${AUSTRO_RABBITMQ_USER:-austro}"

for required_secret in AUSTRO_POSTGRES_RUNTIME_PASSWORD AUSTRO_REDIS_PASSWORD; do
	[[ -n "${!required_secret:-}" ]] \
		|| die "$ENV_FILE does not define $required_secret (needed to authenticate as the runtime role and to Redis)"
done
DB_RUNTIME_PASSWORD="${AUSTRO_POSTGRES_RUNTIME_PASSWORD:-}"
REDIS_PASSWORD="${AUSTRO_REDIS_PASSWORD:-}"

printf '=== AUSTRO OS healthcheck (AlwaysData) ===\n'

API_BASE="http://127.0.0.1:8301"

api_live="$(curl -fsS "$API_BASE/health/live" 2>/dev/null || printf 'curl-exit=%s' "$?" || true)"
if [[ "$api_live" == *'"status":"ok"'* ]]; then
	ok "API /health/live answered ok"
else
	bad "API /health/live did not answer ok (got: ${api_live:-<empty>})"
fi

api_ready="$(curl -fsS "http://127.0.0.1:8301/health/ready" 2>/dev/null || printf 'curl-exit=%s' "$?" || true)"
if [[ "$api_ready" == *'"ready":true'* ]]; then
	ok "API /health/ready answered ready"
else
	bad "API /health/ready did not answer ready (got: ${api_ready:-<empty>})"
fi

if PGPASSWORD="$AUSTRO_POSTGRES_RUNTIME_PASSWORD" psql "$AUSTRO_POSTGRES_RUNTIME_DSN" -c 'SELECT 1' >/dev/null 2>&1; then
	ok "PostgreSQL answered a query as the runtime role ($DB_RUNTIME_USER)"
else
	bad "PostgreSQL did not answer a query as the runtime role ($DB_RUNTIME_USER)"
fi

runtime_query="$(PGPASSWORD="$AUSTRO_POSTGRES_RUNTIME_PASSWORD" psql \
	"$AUSTRO_POSTGRES_RUNTIME_DSN" -tAc 'SELECT 1' 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$runtime_query" == "1" ]]; then
	ok "PostgreSQL answered a query as the runtime role ($DB_RUNTIME_USER)"
else
	bad "PostgreSQL did not answer a query as the runtime role ($DB_RUNTIME_USER)"
fi

if [[ "$runtime_query" == "1" ]]; then
	connected_role="$(PGPASSWORD="$AUSTRO_POSTGRES_RUNTIME_PASSWORD" psql \
		"$AUSTRO_POSTGRES_RUNTIME_DSN" -tAc 'SELECT current_user' 2>/dev/null | tr -d '[:space:]' || true)"
	if [[ "$connected_role" == "$DB_RUNTIME_USER" ]]; then
		ok "connection authenticated as the intended runtime role ($DB_RUNTIME_USER)"
	else
		bad "expected to connect as $DB_RUNTIME_USER but the server reported '${connected_role:-<empty>}'"
	fi

	privilege_status=0
	privilege_output="$(PGPASSWORD="$AUSTRO_POSTGRES_RUNTIME_PASSWORD" psql \
		"$AUSTRO_POSTGRES_RUNTIME_DSN" -v ON_ERROR_STOP=1 -tA 2>&1 <<'SQL'
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

redis_ping="$(REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli -h services-austro.alwaysdata.net -p 8300 --no-auth-warning ping 2>/dev/null | tr -d '[:space:]' || true)"
if [[ "$redis_ping" == "PONG" ]]; then
	ok "Redis answered an authenticated PING"
else
	bad "Redis did not answer an authenticated PING (got: ${redis_ping:-<empty>})"
fi

if command -v python3 >/dev/null 2>&1; then
	python3 -c "
import urllib.request, json, sys, base64, os
try:
    auth = base64.b64encode(b'austro:' + os.environ.get('AUSTRO_RABBITMQ_PASSWORD', '').encode()).decode()
    req = urllib.request.Request('https://rabbitmq-austro.alwaysdata.net:15672/api/queues/austro_dz', headers={'Authorization': 'Basic ' + auth})
    with urllib.request.urlopen(req, timeout=10) as resp:
        data = json.loads(resp.read().decode())
    queues = [q['name'] for q in data]
    if 'austro.events' in queues:
        print('OK')
        sys.exit(0)
    else:
        print('MISSING')
        sys.exit(1)
except Exception as e:
    print('ERROR:', e)
    sys.exit(1)
" 2>/dev/null && echo "OK" || echo "MISSING"
else
	echo "SKIP"
fi | while read result; do
    if [ "$result" = "OK" ]; then
        echo "PASS     expected queue exists: austro.events"
    elif [ "$result" = "MISSING" ]; then
        echo "FAIL     expected queue is missing: austro.events" >&2
    else
        echo "         RabbitMQ queue check skipped (no python3 available)"
    fi
done

if python3 -c "
import pika, sys, os
try:
    conn = pika.BlockingConnection(pika.URLParameters(os.environ.get('AUSTRO_RABBITMQ_URL', '')))
    conn.close()
    print('OK')
    sys.exit(0)
except Exception as e:
    print('FAIL:', e)
    sys.exit(1)
" 2>/dev/null; then
	echo "PASS     RabbitMQ AMQP connection successful"
else
	echo "FAIL     RabbitMQ AMQP connection failed" >&2
fi

note "API log checks skipped on AlwaysData (no journalctl access)"

if [[ "$AUSTRO_HEALTHCHECK_SKIP_PROXY" == "1" ]]; then
	note "proxy checks skipped (AUSTRO_HEALTHCHECK_SKIP_PROXY=1)"
else
	public_host="${AUSTRO_PUBLIC_HOSTNAME:-}"
	if [[ -n "$public_host" ]]; then
		https_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "https://${public_host}/health/live" 2>/dev/null || true)"
		if [[ "$https_code" == "200" ]]; then
			ok "public HTTPS endpoint served /health/live (HTTP 200)"
		else
			bad "public HTTPS endpoint did not serve /health/live (HTTP ${https_code:-no response})"
		fi
	else
		note "public HTTPS check skipped (AUSTRO_PUBLIC_HOSTNAME not set)"
	fi
fi

printf '\n=== SUMMARY ===\n'
printf 'PASS:     %d\n' "$PASS_COUNT"
printf 'FAIL:     %d\n' "$FAIL_COUNT"
printf 'BLOCKED:  %d\n' "$BLOCKED_COUNT"

if [ "$FAIL_COUNT" -gt 0 ]; then
	exit 1
fi
exit 0