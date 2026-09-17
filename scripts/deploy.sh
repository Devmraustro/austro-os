#!/usr/bin/env bash
#
# AUSTRO OS — production deployment.
#
# Validates the deployment configuration, takes a pre-deployment backup when
# there is something to back up, then brings the stack up in dependency order
# and proves it is healthy before reporting success.
#
#   scripts/deploy.sh
#
# Start order (and why it is this order):
#   postgres -> redis -> rabbitmq -> api -> worker -> reverse proxy
#
#   * The three infrastructure services first: the API exits at startup if it
#     cannot reach any of them, so starting it earlier only produces a crash
#     loop.
#   * The API before the worker. Both processes apply the schema bootstrap at
#     startup and that bootstrap takes no advisory lock, so two processes
#     racing it against a cold database would collide on the same DDL. The
#     compose file gates the worker on the API being healthy for the same
#     reason; this script mirrors that order rather than relying on it.
#   * The reverse proxy last, so it never serves 502s at a half-built stack.
#
# Every step is bounded and every failure returns non-zero. Nothing here prints
# a secret value: checks report the NAME of a setting, never its contents.

set -Eeuo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

COMPOSE_FILE="docker-compose.production.yml"
ENV_FILE="${AUSTRO_ENV_FILE:-.env.production}"

# Reverse proxy to start. `AUSTRO_PROXY_SERVICE=reverse-proxy-caddy` selects the
# automatic-TLS variant, which lives behind the compose profile `caddy`.
PROXY_SERVICE="${AUSTRO_PROXY_SERVICE:-reverse-proxy}"
COMPOSE_PROFILE_ARGS=()
if [[ "$PROXY_SERVICE" == "reverse-proxy-caddy" ]]; then
    COMPOSE_PROFILE_ARGS=(--profile caddy)
fi

INFRA_SERVICES=(postgres redis rabbitmq)

log()  { printf '[deploy] %s\n' "$*"; }
warn() { printf '[deploy] WARNING: %s\n' "$*" >&2; }
die()  { printf '[deploy] ERROR: %s\n' "$*" >&2; exit 1; }

# compose <args...>
# The --env-file is required for interpolation; it is the same file the services
# receive through env_file. Passing it everywhere keeps the validated values and
# the deployed values identical.
compose() {
    docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
        ${COMPOSE_PROFILE_ARGS[@]+"${COMPOSE_PROFILE_ARGS[@]}"} "$@"
}

# ---------------------------------------------------------------------------
# 1. Preconditions
# ---------------------------------------------------------------------------
log "checking prerequisites"
command -v docker >/dev/null 2>&1 || die "docker is not installed or not on PATH"
docker compose version >/dev/null 2>&1 || die "the docker compose plugin is not available"
docker info >/dev/null 2>&1 || die "cannot talk to the docker daemon (is it running, and is this user allowed to reach it?)"

[[ -f "$COMPOSE_FILE" ]] || die "missing $COMPOSE_FILE (run this from the repository root)"
[[ -f "$ENV_FILE" ]] || die "missing $ENV_FILE — copy .env.example to $ENV_FILE and fill in every REQUIRED value"

# ---------------------------------------------------------------------------
# 2. Configuration validation
# ---------------------------------------------------------------------------
# The values are loaded into this shell so they can be checked before anything
# is created, and so compose interpolation sees exactly the values that were
# validated (the shell environment takes precedence over --env-file).
log "validating $ENV_FILE"
set -a
# shellcheck source=/dev/null
. "$ENV_FILE"
set +a

# validate_secret <NAME> <min-length>
# Reports the setting NAME only. The value is never echoed, not even a prefix:
# a truncated secret in a build log is still a secret in a build log.
validate_secret() {
    local name="$1" min_length="$2" value="${!1:-}"
    [[ -n "$value" ]] || die "$name is empty or unset"
    local lowered
    lowered="$(printf '%s' "$value" | tr '[:upper:]' '[:lower:]')"
    case "$lowered" in
        *change-me*|*changeme*|*example*|*placeholder*|*"your-"*)
            die "$name is still a placeholder — generate a real value with: openssl rand -hex 32" ;;
    esac
    if (( ${#value} < min_length )); then
        die "$name is shorter than the required $min_length characters"
    fi
}

validate_secret AUSTRO_POSTGRES_PASSWORD 16
validate_secret AUSTRO_POSTGRES_RUNTIME_PASSWORD 16
validate_secret AUSTRO_REDIS_PASSWORD 16
validate_secret AUSTRO_RABBITMQ_PASSWORD 16
validate_secret AUSTRO_JWT_SECRET 32
validate_secret AUSTRO_JWT_REFRESH_SECRET 32
validate_secret AUSTRO_FOUNDER_USERNAME 1
validate_secret AUSTRO_FOUNDER_PASSWORD 16

# The database principals must not share a credential. The runtime role is the
# identity that serves traffic; if it were also the owner, PostgreSQL would skip
# row level security for every table it owns and workspace isolation would be
# nominal only.
[[ "${AUSTRO_POSTGRES_PASSWORD}" != "${AUSTRO_POSTGRES_RUNTIME_PASSWORD}" ]] \
    || die "AUSTRO_POSTGRES_PASSWORD and AUSTRO_POSTGRES_RUNTIME_PASSWORD must differ (the runtime role must not be the schema owner)"

# The two signing keys must differ, or a refresh token is a valid access token.
[[ "${AUSTRO_JWT_SECRET}" != "${AUSTRO_JWT_REFRESH_SECRET}" ]] \
    || die "AUSTRO_JWT_SECRET and AUSTRO_JWT_REFRESH_SECRET must differ (a shared key would let a refresh token be replayed as an access token)"

for name in AUSTRO_POSTGRES_USER AUSTRO_POSTGRES_DB AUSTRO_POSTGRES_RUNTIME_USER; do
    value="${!name:-}"
    if [[ -n "$value" ]] && ! [[ "$value" =~ ^[a-z_][a-z0-9_]{0,62}$ ]]; then
        # Role and database names cannot be bound as DDL parameters; the
        # application checks role names against this same pattern before
        # interpolating them, so a name that fails here would fail later.
        die "$name must match ^[a-z_][a-z0-9_]{0,62}$ (it is interpolated into DDL, which cannot use bind parameters)"
    fi
done

# AI and publishing adapters: selecting a real backend makes its settings
# required. Checking here turns a startup crash into a deployment-time error
# naming the missing setting.
case "${AUSTRO_AI_BACKEND:-stub}" in
    stub) ;;
    local)
        [[ -n "${AUSTRO_AI_MODEL:-}"    ]] || die "AUSTRO_AI_BACKEND=local requires AUSTRO_AI_MODEL"
        [[ -n "${AUSTRO_AI_BASE_URL:-}" ]] || die "AUSTRO_AI_BACKEND=local requires AUSTRO_AI_BASE_URL" ;;
    openai-compatible)
        for name in AUSTRO_AI_MODEL AUSTRO_AI_BASE_URL AUSTRO_AI_API_KEY; do
            [[ -n "${!name:-}" ]] || die "AUSTRO_AI_BACKEND=openai-compatible requires $name"
        done ;;
    *) die "AUSTRO_AI_BACKEND='${AUSTRO_AI_BACKEND}' is not a supported backend (stub, local, openai-compatible)" ;;
esac

case "${AUSTRO_PUBLISH_BACKEND:-stub}" in
    stub) ;;
    generic-http)
        for name in AUSTRO_PUBLISH_WEBHOOK_URL AUSTRO_PUBLISH_TOKEN; do
            [[ -n "${!name:-}" ]] || die "AUSTRO_PUBLISH_BACKEND=generic-http requires $name"
        done ;;
    *) die "AUSTRO_PUBLISH_BACKEND='${AUSTRO_PUBLISH_BACKEND}' is not a supported backend (stub, generic-http)" ;;
esac

log "configuration validated"

# ---------------------------------------------------------------------------
# 3. Compose validation
# ---------------------------------------------------------------------------
# Resolves the whole file (interpolation, anchors, schema) before anything is
# created. This is the authoritative syntax gate; scripts/verify-production-deployment.sh
# deliberately does not pretend to be it.
log "validating compose configuration"
if ! compose config -q; then
    die "docker compose rejected $COMPOSE_FILE (run 'docker compose --env-file $ENV_FILE -f $COMPOSE_FILE config' to see the resolved file)"
fi

# ---------------------------------------------------------------------------
# 4. Reverse proxy prerequisites
# ---------------------------------------------------------------------------
# AUSTRO_PUBLIC_HOSTNAME is required for either variant: the Caddyfile uses it as
# the site address, and scripts/healthcheck.sh uses it to resolve the HTTPS probe
# against the local proxy.
[[ -n "${AUSTRO_PUBLIC_HOSTNAME:-}" ]] \
    || die "AUSTRO_PUBLIC_HOSTNAME is required (the public hostname the proxy serves)"

if [[ "$PROXY_SERVICE" == "reverse-proxy-caddy" ]]; then
    # The compose file does not declare these with `:?`, because a required
    # variable on a profiled service breaks every compose command that merely
    # resolves the file — including deployments that never start that profile.
    # The requirement is therefore enforced here, where it applies.
    [[ -n "${AUSTRO_ACME_EMAIL:-}" ]] \
        || die "AUSTRO_ACME_EMAIL is required when AUSTRO_PROXY_SERVICE=reverse-proxy-caddy"
    log "reverse proxy: Caddy with automatic TLS for ${AUSTRO_PUBLIC_HOSTNAME}"
fi

if [[ "$PROXY_SERVICE" == "reverse-proxy" ]]; then
    CERT_DIR="${AUSTRO_TLS_CERT_DIR:-./deploy/tls}"
    for f in fullchain.pem privkey.pem; do
        [[ -f "$CERT_DIR/$f" ]] || die "missing TLS file $CERT_DIR/$f — the reverse proxy cannot start without it (see docs/production-configuration.md)"
    done
    log "TLS material present in $CERT_DIR"
fi

# ---------------------------------------------------------------------------
# 5. Pre-deployment backup
# ---------------------------------------------------------------------------
# Only meaningful when a database is already running: on a first deployment
# there is nothing to lose, and saying so is better than reporting a skipped
# backup as if it had happened.
if docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" ps -q postgres 2>/dev/null | grep -q .; then
    log "existing postgres container detected — taking a pre-deployment backup"
    if [[ -x scripts/backup.sh ]]; then
        if ! scripts/backup.sh; then
            die "pre-deployment backup failed; refusing to deploy over an unbacked-up database"
        fi
    else
        die "scripts/backup.sh is missing or not executable; refusing to deploy without a pre-deployment backup"
    fi
else
    log "no existing postgres container — first deployment, no backup to take"
fi

# ---------------------------------------------------------------------------
# 6. Bring the stack up in dependency order
# ---------------------------------------------------------------------------
# wait_for_healthy <service> <timeout-seconds>
# Polls the container's own healthcheck rather than sleeping a fixed amount, so
# the wait ends when the service is actually ready and fails at a bound when it
# never becomes ready.
wait_for_healthy() {
    local service="$1" timeout="${2:-180}" waited=0 interval=3 container_id status

    while (( waited < timeout )); do
        container_id="$(compose ps -q "$service" 2>/dev/null || true)"
        if [[ -n "$container_id" ]]; then
            status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container_id" 2>/dev/null || echo unknown)"
            case "$status" in
                healthy) log "$service is healthy"; return 0 ;;
                unhealthy)
                    printf '[deploy] ERROR: %s reported unhealthy. Recent logs:\n' "$service" >&2
                    compose logs --tail=40 "$service" >&2 || true
                    return 1 ;;
            esac
        fi
        sleep "$interval"
        waited=$((waited + interval))
    done

    printf '[deploy] ERROR: %s did not become healthy within %ss (last status: %s). Recent logs:\n' \
        "$service" "$timeout" "${status:-not-created}" >&2
    compose logs --tail=40 "$service" >&2 || true
    return 1
}

log "starting infrastructure: ${INFRA_SERVICES[*]}"
compose up -d --no-recreate "${INFRA_SERVICES[@]}"
for service in "${INFRA_SERVICES[@]}"; do
    wait_for_healthy "$service" 240 || die "$service failed to start"
done

log "starting api"
compose up -d --no-recreate api
# Generous: on a cold database the first start applies the schema migrations and
# provisions the RLS role topology before it opens the listener.
wait_for_healthy api 420 || die "api failed to start"

log "starting worker"
compose up -d --no-recreate worker
wait_for_healthy worker 180 || die "worker failed to start"

log "starting reverse proxy ($PROXY_SERVICE)"
compose up -d --no-recreate "$PROXY_SERVICE"
wait_for_healthy "$PROXY_SERVICE" 120 || die "reverse proxy failed to start"

# ---------------------------------------------------------------------------
# 7. Verify the running topology
# ---------------------------------------------------------------------------
# "The containers started" is not the same claim as "the system works". This
# runs the real per-dependency checks.
log "running healthcheck"
if [[ -x scripts/healthcheck.sh ]]; then
    scripts/healthcheck.sh || die "healthcheck failed — the stack is up but not verified"
else
    die "scripts/healthcheck.sh is missing or not executable"
fi

log "deployment complete"
compose ps
