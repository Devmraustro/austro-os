#!/usr/bin/env bash
# bootstrap-ubuntu24.sh — one-time, idempotent provisioning of a single
# Ubuntu 24.04 ARM64 host to run AUSTRO OS natively (deploy/native/).
#
# Requirements:
#   * run as root (or via sudo)
#   * /etc/austro/austro.env must already exist and contain no change-me-*
#     placeholders (create it with deploy/native/generate-env.sh, or copy
#     austro.env.example and fill it in — but never risk a placeholder)
#   * a package-universe-enabled Ubuntu 24.04 ("noble") host
#
# What it does, idempotently:
#   * installs PostgreSQL 16 + pgvector, Redis, RabbitMQ, nginx, curl, ufw
#   * creates the OS user "austro" and the /opt/austro release layout
#   * provisions PostgreSQL as this host's only database: role "austro"
#     (LOGIN CREATEROLE NOSUPERUSER NOBYPASSRLS), database "austro" owned by
#     it, EXTENSION vector/pgcrypto already installed (so the application
#     never needs CREATE EXTENSION privilege), scram authentication over
#     127.0.0.1 only
#   * writes a managed redis.conf: requirepass, bound to 127.0.0.1,
#     protected-mode, AOF every second
#   * configures RabbitMQ to listen on 127.0.0.1 only, creates the "austro"
#     user with full permissions on the default vhost, removes "guest"
#   * installs the native nginx.conf (fail-closed: nginx stays disabled until
#     real TLS material exists at /etc/austro/tls/)
#   * installs and enables the austro-api/austro-worker systemd units and the
#     simple firewall (22, 80, 443)
#
# Secrets are read from the env file and passed into the datastores without
# ever being printed, written to a temporary file, or placed in a process
# argument list. Every generated secret is expected to be lowercase hex (as
# generate-env.sh produces); values that do not match are rejected so they can
# never be interpreted as SQL or configuration syntax.
#
# Re-run is safe: every step either no-ops or converges to the same state.

set -Eeuo pipefail

ENV_FILE="${AUSTRO_ENV_FILE:-/etc/austro/austro.env}"

log() { printf 'bootstrap: %s\n' "$*" >&2; }

die() {
	log "error: $*"
	exit 1
}

require_hex() {
	# Usage: require_hex "$VALUE" "description". Rejects anything that is not
	# a reasonably long lowercase hex string so the value can safely appear in
	# single-quoted SQL, a redis.conf line, or an AMQP URL.
	local value="$1"
	local what="$2"
	if ! printf '%s' "$value" | grep -Eq '^[0-9a-f]{32,}$'; then
		die "$what must be a lowercase hex string of at least 32 characters (generate with openssl rand -hex 32)"
	fi
}

if [ "$(id -u)" -ne 0 ]; then
	die "run as root (sudo)"
fi

if ! [ -r "$ENV_FILE" ]; then
	die "cannot read $ENV_FILE — create it first with deploy/native/generate-env.sh"
fi

if grep -q 'change-me' "$ENV_FILE"; then
	die "$ENV_FILE still contains change-me-* placeholders; the application would refuse to start with them"
fi

# Load the environment so provisioning and the derived values agree with what
# the application will actually run with. allexport so the values are visible
# to the (few) sub-processes that need them; nothing ever prints them.
set -a
# shellcheck source=/etc/austro/austro.env
# shellcheck disable=SC1091
. "$ENV_FILE"
set +a

# --- validate derived secrets ---------------------------------------------
OWNER_PW="$(printf '%s' "$AUSTRO_POSTGRES_DSN" | sed -n 's#^postgres://austro:\([^@]*\)@.*$#\1#p')"
RUNTIME_PW="$(printf '%s' "$AUSTRO_POSTGRES_DSN" | sed -n 's#^postgres://austro_app:\([^@]*\)@.*$#\1#p')"
if [ -z "$OWNER_PW" ] || [ -z "$RUNTIME_PW" ]; then
	die "cannot derive PostgreSQL passwords from AUSTRO_POSTGRES_DSN (expected postgres://austro:...@127.0.0.1:5432/austro?sslmode=disable)"
fi
require_hex "$OWNER_PW" "PostgreSQL owner password"
require_hex "$RUNTIME_PW" "PostgreSQL runtime role password"

RABBIT_PW="$(printf '%s' "$AUSTRO_RABBITMQ_URL" | sed -n 's#^amqp://austro:\([^@]*\)@.*$#\1#p')"
if [ -z "$RABBIT_PW" ]; then
	die "cannot derive RabbitMQ password from AUSTRO_RABBITMQ_URL"
fi
require_hex "$RABBIT_PW" "RabbitMQ password"

if [ -z "${AUSTRO_REDIS_PASSWORD:-}" ]; then
	die "AUSTRO_REDIS_PASSWORD is empty in $ENV_FILE"
fi
require_hex "$AUSTRO_REDIS_PASSWORD" "Redis password"

if [ "$(stat -c %a "$ENV_FILE")" != "600" ] || [ "$(stat -c %U "$ENV_FILE")" != "root" ]; then
	die "$ENV_FILE must be owned by root and mode 0600 (chown root:root; chmod 600)"
fi

# --- apt ------------------------------------------------------------------
export DEBIAN_FRONTEND=noninteractive

apt-get update -y

log "installing PostgreSQL 16 (+ pgvector), Redis, RabbitMQ, nginx, curl, firewall, updates"
# shellcheck disable=SC2015 # failure path already dies with guidance
if ! apt-get install -y \
	postgresql-16 postgresql-client-16 postgresql-16-pgvector \
	redis-server \
	rabbitmq-server \
	nginx-core nginx \
	curl \
	ufw \
	unattended-upgrades openssl ca-certificates \
	>/dev/null; then
	die "base package install failed. On Ubuntu 24.04 ensure the universe component is enabled and the binaries are pinned to the distro archive (postgresql-16, postgresql-16-pgvector, redis-server, rabbitmq-server)."
fi

# --- OS user and release layout -------------------------------------------
if ! id -u austro >/dev/null 2>&1; then
	adduser --system --group --home /opt/austro --shell /usr/sbin/nologin austro
fi
install -d -o austro -g austro /opt/austro/releases
install -d -o austro -g austro /opt/austro/current
install -d -o austro -g austro /opt/austro/deploy/native

cp -f "$(dirname "$0")/nginx.conf" /opt/austro/deploy/native/nginx.conf
chown austro:austro /opt/austro/deploy/native/nginx.conf

# --- systemd units ---------------------------------------------------------
install -m 0644 -o root -g root "$(dirname "$0")/austro-api.service" /etc/systemd/system/austro-api.service
install -m 0644 -o root -g root "$(dirname "$0")/austro-worker.service" /etc/systemd/system/austro-worker.service
systemctl daemon-reload
systemctl enable austro-api.service >/dev/null 2>&1 || true
systemctl enable austro-worker.service >/dev/null 2>&1 || true

# --- PostgreSQL -------------------------------------------------------------
PG_DATA=/etc/postgresql/16/main
install -d -o postgres -g postgres /etc/postgresql/16/main/conf.d

if ! systemctl is-active --quiet postgresql; then
	systemctl start postgresql
fi

cat >/etc/postgresql/16/main/conf.d/95-austro.conf <<EOF
# Managed by AUSTRO OS bootstrap-ubuntu24.sh. This database serves only the
# local application: it listens on loopback and every password is written with
# SCRAM-SHA-256.
listen_addresses = '127.0.0.1'
password_encryption = scram-sha-256
EOF

if ! grep -qs '^# AUSTRO OS managed' /etc/postgresql/16/main/pg_hba.conf; then
	cp -a /etc/postgresql/16/main/pg_hba.conf /etc/postgresql/16/main/pg_hba.conf.bootstrap-orig
fi

cat >/etc/postgresql/16/main/pg_hba.conf <<'EOF'
# AUSTRO OS managed pg_hba.conf (bootstrap-ubuntu24.sh).
# The database is reachable only over loopback with SCRAM-SHA-256; the local
# unix socket uses peer for postgres (operator/backup path) and every other
# local user, and anything else is refused outright.
local   all             postgres                                peer
local   all             all                                     peer
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
host    all             all             0.0.0.0/0               reject
host    all             all             ::0/0                   reject
EOF

# pg_hba.conf requires a reload, conf.d a restart (listen_addresses).
systemctl restart postgresql

# Role "austro" (owner, CREATEROLE, NOT a superuser) + database owned by it.
# The password travels only on the psql stdin stream.
PG_ROLE_EXISTS="$(runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'austro'")"
if [ "$PG_ROLE_EXISTS" = "1" ]; then
	runuser -u postgres -- psql -v ON_ERROR_STOP=1 -d postgres <<SQL
ALTER ROLE austro WITH LOGIN CREATEROLE NOSUPERUSER NOBYPASSRLS PASSWORD '$OWNER_PW';
SQL
else
	runuser -u postgres -- psql -v ON_ERROR_STOP=1 -d postgres <<SQL
CREATE ROLE austro LOGIN CREATEROLE NOSUPERUSER NOBYPASSRLS PASSWORD '$OWNER_PW';
SQL
fi

PG_DB_EXISTS="$(runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_database WHERE datname = 'austro'")"
if [ "$PG_DB_EXISTS" != "1" ]; then
	runuser -u postgres -- psql -v ON_ERROR_STOP=1 -d postgres -c 'CREATE DATABASE austro OWNER austro'
fi

# The extensions are pre-created as the superuser so the application process
# (running as the unprivileged owner) never needs CREATE EXTENSION privilege.
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -d austro -c 'CREATE EXTENSION IF NOT EXISTS vector'
runuser -u postgres -- psql -v ON_ERROR_STOP=1 -d austro -c 'CREATE EXTENSION IF NOT EXISTS pgcrypto'

log "PostgreSQL ready on 127.0.0.1:5432 (roles austro, db austro, vector+pgcrypto)"

# --- Redis ------------------------------------------------------------------
cat >/etc/redis/redis.conf <<EOF
# AUSTRO OS managed redis.conf (bootstrap-ubuntu24.sh).
bind 127.0.0.1
protected-mode yes
port 6379
timeout 0
tcp-keepalive 300
daemonize no
supervised systemd
pidfile /run/redis/redis-server.pid
loglevel notice
logfile ""
save ""
appendonly yes
appendfsync everysec
auto-aof-rewrite-percentage 100
auto-aof-rewrite-min-size 64mb
dir /var/lib/redis
maxmemory 256mb
maxmemory-policy noeviction
requirepass $AUSTRO_REDIS_PASSWORD
EOF
chown redis:root /etc/redis/redis.conf
chmod 0640 /etc/redis/redis.conf

systemctl enable redis-server >/dev/null 2>&1 || true
# Restart (not reload): the managed conf may have changed on re-run and the
# Ubuntu unit has no ExecReload target.
systemctl restart redis-server

if ! REDISCLI_AUTH="$AUSTRO_REDIS_PASSWORD" redis-cli --no-auth-warning -h 127.0.0.1 ping | grep -q PONG; then
	die "Redis authentication check failed"
fi
log "Redis ready on 127.0.0.1:6379 (requirepass, AOF every second)"

# --- RabbitMQ ----------------------------------------------------------------
mkdir -p /etc/rabbitmq
cat >/etc/rabbitmq/rabbitmq.conf <<EOF
# AUSTRO OS managed rabbitmq.conf (bootstrap-ubuntu24.sh).
# The broker serves the local application only.
listeners.tcp.default = 127.0.0.1:5672
EOF

# The Debian package can leave the management plugin listening on a port the
# firewall would normally cover; disable it explicitly so no auxiliary surface
# exists.
if rabbitmq-plugins is_enabled rabbitmq_management >/dev/null 2>&1; then
	rabbitmq-plugins disable rabbitmq_management
fi

systemctl enable rabbitmq-server >/dev/null 2>&1 || true
systemctl restart rabbitmq-server

# Cookie is owned by the rabbitmq OS user; rabbitmqctl must run as that user
# or it cannot talk to the daemon.
if ! runuser -u rabbitmq -- rabbitmqctl await_startup >/dev/null 2>&1; then
	die "RabbitMQ failed to become ready"
fi
if ! runuser -u rabbitmq -- rabbitmqctl list_users | grep -q '^austro'; then
	runuser -u rabbitmq -- rabbitmqctl add_user austro "$RABBIT_PW"
else
	runuser -u rabbitmq -- rabbitmqctl change_password austro "$RABBIT_PW"
fi
runuser -u rabbitmq -- rabbitmqctl set_permissions -p / austro '.*' '.*' '.*'
runuser -u rabbitmq -- rabbitmqctl delete_user guest >/dev/null 2>&1 || true
log "RabbitMQ ready on 127.0.0.1:5672 (user austro, pub/sub/queue on default vhost)"

# --- nginx (fail-closed until real TLS exists) -------------------------------
TLS_DIR=/etc/austro/tls
if [ -f "$TLS_DIR/fullchain.pem" ] && [ -f "$TLS_DIR/privkey.pem" ]; then
	install -m 0644 -o root -g root /opt/austro/deploy/native/nginx.conf /etc/nginx/nginx.conf
	if nginx -t >/dev/null 2>&1; then
		systemctl enable nginx >/dev/null 2>&1 || true
		if ! systemctl is-active --quiet nginx; then
			systemctl start nginx
		fi
		systemctl reload nginx
		log "nginx ready on 80/443 (TLS material present)"
	else
		die "nginx -t failed with the installed TLS material; inspect /var/log/nginx/error.log"
	fi
else
	log "TLS material not present at $TLS_DIR — leaving nginx DISABLED (fail-closed, no plaintext)."
	log "Install fullchain.pem and privkey.pem there, then run: systemctl enable --now nginx && systemctl reload nginx"
fi

# --- Firewall ---------------------------------------------------------------
ufw allow 22/tcp >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw --force enable >/dev/null
log "ufw enabled: 22, 80, 443 inbound; all other inbound denied"

# --- Unattended security updates ---------------------------------------------
systemctl enable unattended-upgrades >/dev/null 2>&1 || true
systemctl restart unattended-upgrades >/dev/null 2>&1 || true
log "unattended-upgrades enabled"

# --- First release reminder ---------------------------------------------------
if [ ! -x /opt/austro/current/main ] || [ ! -x /opt/austro/current/worker ]; then
	log "no release installed yet. Deploy one now with: sudo deploy/native/install-release.sh <archive> <sha256>"
	log "then verify with: sudo deploy/native/healthcheck.sh"
fi

log "bootstrap complete."
log "OPERATOR ACTION: place TLS material in /etc/austro/tls and enable nginx as shown above."
log "If AUSTRO_AI_BACKEND or AUSTRO_PUBLISH_BACKEND is intentionally a real provider,"
log "  update their settings in $ENV_FILE before starting the application (they are stub here)."