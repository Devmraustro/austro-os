# AUSTRO OS — Native ARM64 Deployment (`deploy/native/`)

A zero-container deployment path for a single cloud VM: the API, the worker,
PostgreSQL 16 + pgvector, Redis and RabbitMQ all run as native system services
under systemd on one Ubuntu 24.04 ARM64 host, with nginx as the only public
ingress.

This is a **verified-to-build, hardware-verified-in-CI** deployment layer, not a
replacement for the primary Docker Compose path. See
[docs/free-native-deployment.md](../../docs/free-native-deployment.md) for the
full runbook and [docs/deployment-targets.md](../../docs/deployment-targets.md)
for how this target relates to the others.

```
            Internet
                |
          80/443 v
     +-------------------+
     | nginx (TLS term.) |   /etc/nginx/nginx.conf  (this directory)
     +---------+---------+
               | 127.0.0.1:8080
               v
     +-------------------+
     | AUSTRO OS API     |   austro-api.service    User=austro, /opt/austro/current/main
     +---------+---------+
               |
     +---------+---------+
     | AUSTRO OS Worker  |   austro-worker.service  connects only to RabbitMQ + PostgreSQL
     +---------+---------+
               |
     +---------+---------+---------+
     | PostgreSQL 16     | Redis | RabbitMQ  |   loopback-only, scram/auth each
     | + pgvector        |       |           |
     +-------------------+-------+-----------+
```

## How a release gets onto the VM

1. `.github/workflows/native-arm64-deploy.yml` builds both binaries with
   `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`, verifies they are AArch64 ELF
   executables, and packages them with their checksum.
2. The `deploy` job (manual `workflow_dispatch`, gated on
   `vars.DEPLOY_ENABLED == 'true'`, GitHub environment `native-prod`) copies the
   release archive and `install-release.sh` to the VM over SSH and runs
   `install-release.sh`, which verifies the checksum, installs under
   `/opt/austro/releases/<sha>/`, atomically switches the `current` symlink,
   restarts both units, health-gates, and **rolls back on failure**.

Long-lived application secrets live only on the VM in `/etc/austro/austro.env`
(`0600`), never in the repository and never on a CI runner.

## File map

| File | Purpose |
|---|---|
| `austro.env.example` | Template for `/etc/austro/austro.env` — every value, classified |
| `generate-env.sh` | Creates `/etc/austro/austro.env` from `openssl rand` when absent (never overwrites, never prints) |
| `austro-api.service` | systemd unit for the API |
| `austro-worker.service` | systemd unit for the worker |
| `bootstrap-ubuntu24.sh` | One-time, idempotent host provisioning (packages, roles, loopback-only services, units, firewall) |
| `install-release.sh` | Install/activate a release archive with checksum, health-gate and rollback |
| `nginx.conf` | The complete nginx configuration (TLS, security headers, 301 redirect) |
| `healthcheck.sh` | Shallow-to-deep verification of the running topology |
| `backup.sh` | PostgreSQL dump + verify + retention (and optional off-host copy) |
| `restore.sh` | Destructive, confirmed restore with post-restore topology verification |
| `monitor.sh` | Periodic JSON signal emitter + debounced webhook alerts |
| `README.md` | This file |

## Operating surface

- Everything an operator runs goes through `deploy/native/*.sh`; none of these
  scripts require Docker.
- The scripts read `/etc/austro/austro.env` by default (override with
  `AUSTRO_ENV_FILE`). Values are never printed.
- All four data-plane services bind loopback only: PostgreSQL `127.0.0.1:5432`,
  Redis `127.0.0.1:6379`, RabbitMQ `127.0.0.1:5672`; nginx is the only listener
  on 80/443.

## Status

See [docs/FREE_DEPLOYMENT_CERTIFICATION.md](../../docs/FREE_DEPLOYMENT_CERTIFICATION.md)
for the exact per-capability status. The build and binary-format verification
run in CI; anything that touches a real VM is recorded as `OPERATOR
CONFIGURATION REQUIRED` until an operator has actually run it.