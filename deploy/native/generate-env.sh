#!/usr/bin/env bash
# generate-env.sh — create /etc/austro/austro.env with freshly generated secrets.
#
# Never overwrites an existing file. Uses openssl rand -hex for every secret so
# the values are URI-safe. No secret is ever echoed to the terminal or written
# to a log; the founder password is read interactively (or from the
# AUSTRO_FOUNDER_PASSWORD / AUSTRO_FOUNDER_USERNAME environment variables).
#
# Usage:
#   sudo deploy/native/generate-env.sh            # writes /etc/austro/austro.env
#   sudo AUSTRO_ENV_FILE=/srv/austro.env deploy/native/generate-env.sh
#
# After generation, edit /etc/austro/austro.env to replace the PUBLIC_HOSTNAME
# placeholder, then run bootstrap-ubuntu24.sh.

set -Eeuo pipefail

ENV_FILE="${AUSTRO_ENV_FILE:-/etc/austro/austro.env}"

log() { printf 'generate-env: %s\n' "$*" >&2; }

die() {
	log "error: $*"
	exit 1
}

if [ "$(id -u)" -ne 0 ]; then
	die "run as root (sudo): the target file is root-owned 0600"
fi

if [ -e "$ENV_FILE" ]; then
	die "refusing to overwrite existing $ENV_FILE (edit it or remove it first)"
fi

if ! command -v openssl >/dev/null 2>&1; then
	die "openssl is required to generate secrets (apt-get install openssl)"
fi

if ! command -v install >/dev/null 2>&1; then
	die "coreutils install(1) is missing"
fi

# Founder credentials: read from the environment when supplied, otherwise ask.
# AUSTRO_ENV (from the app config) is not validated against the founder pair;
# the application runtime validates both-or-neither, so generate both here only
# when the operator opts into founder bootstrap now.
FOUNDER_USERNAME="${AUSTRO_FOUNDER_USERNAME:-}"
FOUNDER_PASSWORD="${AUSTRO_FOUNDER_PASSWORD:-}"

if [ -z "$FOUNDER_USERNAME" ]; then
	read -r -p "Founder username (enter to skip founder bootstrap): " FOUNDER_USERNAME || FOUNDER_USERNAME=""
fi

if [ "$FOUNDER_USERNAME" != "" ] && [ -z "$FOUNDER_PASSWORD" ]; then
	IFS= read -r -s -p "Founder password (min 16 characters): " FOUNDER_PASSWORD || true
	printf '\n'
	if [ "${#FOUNDER_PASSWORD}" -lt 16 ]; then
		die "founder password must be at least 16 characters"
	fi
fi

if [ "$FOUNDER_USERNAME" != "" ] && [ "$FOUNDER_PASSWORD" = "" ]; then
	die "founder username was given without a password; both or neither are required"
fi

# Machine secrets: hex so every character is URI- and flat-file-safe.
PG_OWNER_PASSWORD="$(openssl rand -hex 32)"
PG_RUNTIME_PASSWORD="$(openssl rand -hex 32)"
REDIS_PASSWORD="$(openssl rand -hex 32)"
RABBITMQ_PASSWORD="$(openssl rand -hex 32)"
JWT_SECRET="$(openssl rand -hex 32)"
JWT_REFRESH_SECRET="$(openssl rand -hex 32)"

mkdir -p "$(dirname "$ENV_FILE")"

umask 077

cat >"$ENV_FILE" <<EOF
AUSTRO_ENV=production
AUSTRO_SERVER_ADDR=127.0.0.1:8080
AUSTRO_PUBLIC_HOSTNAME=change-me-public-hostname

AUSTRO_POSTGRES_DSN=postgres://austro:${PG_OWNER_PASSWORD}@127.0.0.1:5432/austro?sslmode=disable
AUSTRO_POSTGRES_RUNTIME_DSN=postgres://austro_app:${PG_RUNTIME_PASSWORD}@127.0.0.1:5432/austro?sslmode=disable
AUSTRO_POSTGRES_RUNTIME_PASSWORD=${PG_RUNTIME_PASSWORD}

AUSTRO_REDIS_ADDR=127.0.0.1:6379
AUSTRO_REDIS_PASSWORD=${REDIS_PASSWORD}

AUSTRO_RABBITMQ_URL=amqp://austro:${RABBITMQ_PASSWORD}@127.0.0.1:5672/
AUSTRO_RABBITMQ_QUEUE=austro.events

AUSTRO_JWT_SECRET=${JWT_SECRET}
AUSTRO_JWT_REFRESH_SECRET=${JWT_REFRESH_SECRET}

AUSTRO_AI_BACKEND=stub
AUSTRO_PUBLISH_BACKEND=stub

AUSTRO_PG_CLUSTER=16/main
AUSTRO_BACKUP_RETENTION_DAYS=7
AUSTRO_BACKUP_DIR=/var/backups/austro
EOF

if [ "$FOUNDER_USERNAME" != "" ]; then
	cat >>"$ENV_FILE" <<EOF

AUSTRO_FOUNDER_USERNAME=${FOUNDER_USERNAME}
AUSTRO_FOUNDER_PASSWORD=${FOUNDER_PASSWORD}
EOF
fi

chmod 0600 "$ENV_FILE"
chown root:root "$ENV_FILE"

log "wrote $ENV_FILE with freshly generated secrets (mode 0600, owner root)"
log "edit it to set AUSTRO_PUBLIC_HOSTNAME, then run bootstrap-ubuntu24.sh"