# AUSTRO OS — AlwaysData Native Deployment Runbook

This runbook describes the native deployment of AUSTRO OS to an AlwaysData
managed hosting account. Unlike the Ubuntu ARM64 native deployment (which runs
all services on a single VM), this deployment uses AlwaysData managed services
for PostgreSQL, Redis, and RabbitMQ, while running the AUSTRO API and Worker
as native AlwaysData Services.

## Architecture

```
                         Internet
                             |
                       443/80  v
                  +-------------------+
                  | AlwaysData Web    |  HTTPS termination, reverse proxy
                  | (Reverse Proxy)   |  to http://services-austro:8301
                  +---------+---------+
                            | http
                            v
                   +-------------------+
                   | AUSTRO OS API     |  AlwaysData Service
                   | port 8301         |  ~/austro/current/austro-api
                   +---------+---------+
                             |
           +-----------------+------------------+
           |                  |                  |
           v                  v                  v
    +--------------+  +--------------+  +--------------+
    | PostgreSQL   |  | Redis        |  | RabbitMQ     |
    | (AlwaysData) |  | (AlwaysData) |  | (AlwaysData) |
    | austro_os    |  | services-    |  | austro_dz    |
    | austro_app   |  | austro:8300  |  | austro       |
    +--------------+  +--------------+  +--------------+
```

## Key Differences from Ubuntu Native Deployment

| Aspect | Ubuntu Native | AlwaysData |
|--------|--------------|------------|
| PostgreSQL | Self-hosted (systemd) | AlwaysData managed service |
| Redis | Self-hosted (systemd) | AlwaysData Service (port 8300) |
| RabbitMQ | Self-hosted (systemd) | AlwaysData managed |
| Nginx/TLS | Self-hosted (fail-closed) | AlwaysData Web reverse proxy |
| Process Manager | systemd | AlwaysData Service Manager |
| User | austro (OS user) | austro (AlwaysData SSH user) |
| Release Dir | /opt/austro/releases | ~/austro/releases |
| Symlink | /opt/austro/current | ~/austro/current |
| API Port | 8080 (loopback) | 8301 (AlwaysData service port) |
| API Binding | 127.0.0.1:8080 | [::]:8301 (dual-stack) |

## Directory Layout

```
~/
├── austro/
│   ├── current -> releases/224bb24089ccbf74dc08ce9c6b3831918c4157fb/
│   ├── releases/
│   │   └── 224bb24089ccbf74dc08ce9c6b3831918c4157fb/
│   │       ├── austro-api
│   │       └── austro-worker
│   ├── austro.env          # Production environment file (this repo's austro.env.alwaysdata)
│   └── deploy/             # Deployment scripts (optional, for manual ops)
└── ...
```

## Release Process

Releases are built by GitHub Actions (`.github/workflows/phase2-ci.yml`) on every
push to main and on every PR. The `vet-build-unit` job builds both binaries
(`austro-api` and `austro-worker`) for `linux/amd64` and uploads them as the
`austro-x64-release` artifact.

### To Deploy a Release

1. **Download the artifact** from the successful GitHub Actions run:
   - Go to the `phase2-ci` workflow run on the `main` branch
   - Download the `austro-x64-release` artifact
   - Extract `austro-api` and `austro-worker`

2. **Upload binaries to AlwaysData** via SFTP/SCP:
   ```bash
   sftp austro@ssh-austro.alwaysdata.net
   put austro-api
   put austro-worker
   ```

3. **Install the release** on AlwaysData:
   ```bash
   ssh austro@ssh-austro.alwaysdata.net
   ~/austro/deploy/alwaysdata/install-release.sh \
     ~/austro/releases/224bb24089ccbf74dc08ce9c6b3831918c4157fb/austro-api \
     ~/austro/releases/224bb24089ccbf74dc08ce9c6b3831918c4157fb/austro-worker \
     224bb24089ccbf74dc08ce9c6b3831918c4157fb
   ```

   The install script will:
   - Verify binary checksums
   - Create the release directory
   - Atomically switch the `~/austro/current` symlink
   - Restart the API and Worker services via AlwaysData service manager

4. **Verify the deployment**:
   ```bash
   ~/austro/deploy/alwaysdata/verify-alwaysdata.sh
   ```

## Environment Configuration

The production environment file is `~/austro/austro.env` (copied from
`deploy/alwaysdata/austro.env.alwaysdata` and filled with real secrets).

Key settings for AlwaysData:

| Variable | Value | Notes |
|----------|-------|-------|
| `AUSTRO_ENV` | `production` | |
| `AUSTRO_SERVER_ADDR` | `[::]:8301` | AlwaysData service port |
| `AUSTRO_POSTGRES_PREPROVISIONED_ROLES` | `true` | **Required** for AlwaysData |
| `AUSTRO_POSTGRES_DSN` | `postgres://austro:...@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require` | Owner DSN |
| `AUSTRO_POSTGRES_RUNTIME_DSN` | `postgres://austro_app:...@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require` | Runtime DSN |
| `AUSTRO_POSTGRES_ADMIN_DSN` | `postgres://austro_app_admin:...@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require` | Admin DSN |
| `AUSTRO_REDIS_ADDR` | `services-austro.alwaysdata.net:8300` | AlwaysData Redis |
| `AUSTRO_RABBITMQ_URL` | `amqp://austro:...@rabbitmq-austro.alwaysdata.net:5672/austro_dz` | vhost `austro_dz` |
| `AUSTRO_AI_BACKEND` | `stub` | Production uses stub unless real AI configured |
| `AUSTRO_PUBLISH_BACKEND` | `stub` | Production uses stub unless real publisher configured |

## Service Management

AlwaysData uses its own service manager (not systemd). Services are configured
via the AlwaysData web panel or CLI.

### API Service
- **Type**: Native program (long-running)
- **Command**: `~/austro/current/austro-api`
- **Working Directory**: `/home/austro/current`
- **Port**: 8301 (bound to `[::]:8301`)
- **Environment File**: `~/austro/austro.env`
- **Auto-restart**: Enabled

### Worker Service
- **Type**: Native program (long-running)
- **Command**: `~/austro/current/austro-worker`
- **Working Directory**: `/home/austro/current`
- **Environment File**: `~/austro/austro.env`
- **Auto-restart**: Enabled

## Database Bootstrap

The application performs its own schema bootstrap on startup using the
owner DSN (`AUSTRO_POSTGRES_DSN`). In pre-provisioned mode:

1. The owner role `austro` creates extensions (`vector`, `pgcrypto`)
2. Creates/updates schema (tables, indexes, RLS policies)
3. Verifies pre-provisioned roles exist with correct attributes
4. Grants permissions to runtime and admin roles
5. Switches to runtime DSN for serving traffic

The bootstrap is idempotent and runs on every API startup.

## Health Checks

- **Liveness**: `http://127.0.0.1:8301/health/live` (API process alive)
- **Readiness**: `http://127.0.0.1:8301/health/ready` (DB, Redis, RabbitMQ reachable)
- **Worker**: Verified via queue processing test

## Rollback Procedure

If a release causes issues:

1. SSH to AlwaysData
2. Switch the symlink back to the previous release:
   ```bash
   ln -sfn ~/austro/releases/<previous-sha> ~/austro/current
   ```
3. Restart services via AlwaysData panel or:
   ```bash
   # API and worker will be restarted by AlwaysData service manager
   # when the symlink changes, or manually restart via panel
   ```

The `install-release.sh` script keeps the previous release for instant rollback.

## Troubleshooting

| Issue | Check |
|-------|-------|
| API won't start | Check `~/austro/austro.env` for correct DSNs and passwords |
| DB connection failed | Verify PostgreSQL TLS, credentials, and `austro_app` role exists |
| Redis connection failed | Check `services-austro.alwaysdata.net:8300` and password |
| RabbitMQ connection failed | Verify vhost `austro_dz`, user `austro`, port 5672 |
| Worker not processing | Check worker logs, then both queues: `austro.events` for pipeline/publication events and `austro.executions` (the `AUSTRO_EXECUTION_QUEUE` value) for AI execution dispatches. A worker that consumes only the first while the second backs up means the two processes disagree about the queue name — check `AUSTRO_EXECUTION_QUEUE` is set identically for the API and the worker. |
| Health check fails | Check `/health/ready` for specific failure reason |

## Security Notes

- All secrets in `~/austro/austro.env` (chmod 600)
- PostgreSQL uses TLS (`sslmode=require`)
- Redis requires authentication
- RabbitMQ uses dedicated vhost `austro_dz` and user `austro`
- API binds to `[::]:8301` (dual-stack IPv4/IPv6)
- Reverse proxy handled by AlwaysData Web (HTTPS termination)
- No direct database/Redis/RabbitMQ exposure to internet

## Useful Commands

```bash
# View API logs
tail -f ~/austro/current/austro-api.log  # if logging to file
# or check AlwaysData service logs in web panel

# View worker logs
tail -f ~/austro/current/austro-worker.log

# Check API health
curl -fsS http://127.0.0.1:8301/health/live
curl -fsS http://127.0.0.1:8301/health/ready

# Test database connectivity
psql "postgres://austro_app:...@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require" -c "SELECT 1"

# Test Redis
redis-cli -h services-austro.alwaysdata.net -p 8300 -a <password> PING

# Test RabbitMQ
rabbitmqctl -h rabbitmq-austro.alwaysdata.net list_queues
```