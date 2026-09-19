# AUSTRO OS — Native Operations

Day-to-day operation of the free native deployment path described in
[free-native-deployment.md](free-native-deployment.md). Everything an operator
runs goes through `deploy/native/*.sh` on the VM; none of these scripts need
Docker. All scripts read `/etc/austro/austro.env` by default (override with
`AUSTRO_ENV_FILE`) and never print secret values.

Filenames below are under `deploy/native/` in this repository; copy them to
the VM as the runbooks show, or keep a checkout of this repository there.

---

## Topology at a glance

| Unit / entity | Identity | Listen | Auth |
|---|---|---|---|
| nginx | `nginx.service` | 80/443 public | TLS in `/etc/austro/tls` |
| API | `austro-api.service` | `127.0.0.1:8080` | via nginx only |
| Worker | `austro-worker.service` | none (outbound to PG + broker) | via its units' env |
| PostgreSQL 16 | `postgresql@16-main` | `127.0.0.1:5432` | scram on loopback, peer on socket |
| Redis | `redis-server.service` | `127.0.0.1:6379` | `requirepass` from env |
| RabbitMQ | `rabbitmq-server.service` | `127.0.0.1:5672` | `austro` user, `guest` deleted |

Secrets file: `/etc/austro/austro.env`, root `0600`. Back it up — it is the
root of trust and cannot be regenerated.

---

## Regular verification

Run every check precisely as the CI health-gate does:

```sh
sudo -n bash deploy/native/healthcheck.sh
```

Exit 0 with `PASS` means the topology, the runtime role, RLS enforcement, the
Redis connection, the worker startup, the queue and the proxy are all verified.
`PASS WITH BLOCKED CHECKS` is the expected state while TLS material is absent
(nginx is fail-closed). Exit 1 with `FAIL` lists the failing signal; start with
[native-troubleshooting.md](native-troubleshooting.md).

## Monitoring

`deploy/native/monitor.sh` emits one JSON line per check with the same signal
set as the healthcheck (services, API liveness/readiness, database ready and
runtime role, Redis auth, broker up, queue present/backlog, worker attached,
worker errors, backup freshness, HTTPS, alert delivery, summary). Feed it into
an agent or a cron line:

```cron
*/5 * * * * root /opt/austro/scripts/monitor.sh --webhook https://example.net/hook
```

- Alerts are **debounced** per signal via `/tmp/austro-monitor/last-alert`
  (default re-alert interval 3600 s, override with `AUSTRO_ALERT_INTERVAL`).
- The queue-backlog threshold is 100 (override with `AUSTRO_QUEUE_BACKLOG`).
- Confirm alert delivery with a forced test; `monitor.sh` reports the
  `alert_delivered` signal.

## Backups

`deploy/native/backup.sh` dumps PostgreSQL over the peer-authenticated local
socket as the postgres OS user — the one path that does **not** carry a secret
through the environment.

```sh
sudo -n bash deploy/native/backup.sh
```

- Output: `<dir>/<name>-<timestamp>.sql.gz` plus `<name>.sha256`; default dir
  `/var/backups/austro`, override `AUSTRO_BACKUP_DIR`.
- Verification before it trusts a backup: non-empty, `gzip -t`, and the dump's
  header is checked through a **full-stream** consumer (the `head | grep -q`
  shape would SIGPIPE the decompressor under pipefail).
- Optional off-host copy: when `AUSTRO_BACKUP_S3_BUCKET` is set, the archive
  and checksum are pushed with the AWS CLI (`aws s3 cp --no-progress`).
- Retention: default 14 days, override `AUSTRO_BACKUP_RETENTION_DAYS`.

Schedule it nightly:

```cron
0 3 * * * root /opt/austro/scripts/backup.sh
```

Check freshness in monitoring via the `backup_fresh` signal (24 h window).

## Restore

`deploy/native/restore.sh` is destructive by design and enforces a confirmation
before touching anything: pass `AUSTRO_RESTORE_CONFIRM=yes` or answer the
interactive prompt by typing the database name.

```sh
sudo -n bash deploy/native/restore.sh /var/backups/austro/austro-2026-09-18-0300.sql.gz
```

Order of operations:

1. Validates the artifact (signature of the file, checksum, gzip, dump header,
   **before** any service stops).
2. Stops the worker and the API.
3. Restores with `gzip -dc | runuser -u postgres -- psql -v ON_ERROR_STOP=1`.
4. Starts the API and waits for `/health/live` (bounded, default 420 s).
5. Verifies the restored schema and the security invariants: the 12 tables,
   the `vector` extension, RLS enabled and forced on every table, the runtime
   role a non-privileged non-owner, the audit schema present.
6. Starts the worker and runs a full `healthcheck.sh` before reporting success.

## Releases and rollback

`deploy/native/install-release.sh <archive> <sha256>` is the single entrypoint
for putting a release live:

1. Verifies the archive's 64-hex SHA-256 against the explicit argument.
2. Extracts to `/opt/austro/releases/<sha>/`, chown `austro`.
3. Switches the `current` symlink atomically (`current.new` → `mv -Tf`).
4. Restarts `austro-api.service`, polls `/health/live` (bounded), then
   restarts `austro-worker.service`.
5. Runs `healthcheck.sh`.
6. On any failure: rolls back to the previous release, restarts both units,
   re-runs the healthcheck, and exits non-zero.
7. Prunes old releases, keeping the current and previous one.

Always record the `<sha>` you ran. To roll back the *next* release without it,
point at the archive the previous run left on the VM
(`/tmp/austro-deploy/`) or rebuild from the repository history.

## Proxy and TLS

- nginx is fail-closed: it is disabled until
  `/etc/austro/tls/fullchain.pem` + `privkey.pem` exist. Recovering:
  place the material, `nginx -t`, then `systemctl enable --now nginx`.
- Certificates are not managed or auto-renewed by this repository. Renewal is
  the operator's job (your ACME client or your provider). After renewal,
  `nginx -s reload` is enough; `healthcheck.sh` will re-verify.
- Verify the served configuration yourself from off-box:
  `curl -fsSI https://<host>/.well-known/health/live` (or the identity
  endpoint) should present your certificate and HSTS.

## Hygiene

- Only 22/80/443 are open (`ufw status`).
- Secrets never appear in git, in CI logs, or in `ps` output: env file is
  `0600`; interactive prompts use `read -r -s`; RabbitMQ and PostgreSQL
  provisioning feed passwords over stdin, never argv.
- `unattended-upgrades` applies security updates; reboot is occasionally
  needed, so the healthy state must be reproducible by re-running
  `bootstrap-ubuntu24.sh` (idempotent) and `healthcheck.sh`.

See [native-troubleshooting.md](native-troubleshooting.md) when a check fails
and [deployment-targets.md](deployment-targets.md) for scope.