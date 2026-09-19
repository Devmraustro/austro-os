# AUSTRO OS — Native Troubleshooting

First-response runbook for the free native deployment path
([free-native-deployment.md](free-native-deployment.md),
[native-operations.md](native-operations.md)). Start with the AS PROBLEM →
TRY → IF STILL BROKEN ladder and always finish with:

```sh
sudo -n bash deploy/native/healthcheck.sh
```

The healthcheck output names the failing signal; every row in this document
maps a symptom to that signal where one exists. Unit names:
`austro-api`, `austro-worker`, `postgresql@16-main`, `redis-server`,
`rabbitmq-server`, `nginx`.

---

## Services: fail to start or crash

| Symptom | Signal | Try | If still broken |
|---|---|---|---|
| `systemctl status austro-api` → failed | `services` | `journalctl -u austro-api -n 3000 --no-pager`; look for the startup reason | Confirm `/etc/austro/austro.env` is complete and valid per `deploy/native/austro.env.example`; re-provision with `bootstrap-ubuntu24.sh` (idempotent) |
| API restart loop | `services` | `systemctl reset-failed austro-api; systemctl restart austro-api` | The unit has `Restart=on-failure`; a repeated crash is a real startup defect — capture the log before restarting |
| Worker never attaches | `worker_attached` | `journalctl -u austro-worker -n 3000 --no-pager` | The unit waits for `/health/live` before starting; check the API is actually reachable on `127.0.0.1:8080` (`curl -fsS http://127.0.0.1:8080/health/live`) |
| Unit enabled but inert after reboot | `services` | `systemctl is-enabled austro-api austro-worker nginx` | Boot order: units use `After=network-online.target`; a Netplan issue can delay it — `systemctl status network-online.target` |

## API: reachable mildly, unhealthy deeply

| Symptom | Signal | Try | If still broken |
|---|---|---|---|
| `/health/live` OK but `/health/ready` fails | `api_ready` | Look for the specific dependency in the ready response | A ready failure is a datastore reachability/auth problem — follow the datastore rows below |
| API refuses startup, `database-topology-ready` missing from log | `database_runtime` | `grep 'database-topology-ready' <(journalctl -u austro-api -n 3000)` | The runtime role failed a bootstrap invariant; verify `AUSTRO_POSTGRES_RUNTIME_DSN` in the env file (should be distinct user `austro_app`, not the owner) |
| `database-runtime-security-failed` appears in the log | `database_runtime` | Look directly above the line in `journalctl -u austro-api -n 3000` | Check the RLS/privilege DO-block results via `deploy/native/healthcheck.sh`'s runtime section |

## Datastores

| Symptom | Signal | Try | If still broken |
|---|---|---|---|
| PostgreSQL down | `database_ready` | `systemctl status postgresql@16-main`; `runuser -u postgres -- pg_isready` | Re-provision with bootstrap; it is idempotent. Verify data survives: bootstrap never touches data on an existing cluster |
| PostgreSQL auth fail on loopback | `database_ready` | `PGPASSWORD='<runtime pw>' psql -h 127.0.0.1 -U austro_app -d austro -c 'SELECT 1'` as root | Confirm `pg_hba.conf` is the bootstrap-managed one (peer on socket, scram on loopback) — `pg_hba_file_rules` via the postgres peer socket |
| Redis down / auth | `redis_auth` | `systemctl status redis-server`; `REDISCLI_AUTH='<pw>' redis-cli --no-auth-warning ping` from the host | The managed conf enforces `requirepass`; a rogue conf would explain auth failures — compare with `deploy/native/` docs and relocate |
| RabbitMQ down | `rabbitmq_up` | `systemctl status rabbitmq-server`; `runuser -u rabbitmq -- rabbitmqctl status` | Log file `/var/log/rabbitmq/*.log`; port 5672 must be loopback |
| RabbitMQ rejects the app user | `rabbitmq_up` | `runuser -u rabbitmq -- rabbitmqctl authenticate_user austro '<pw>'` | Re-provision fixes the user/password/permissions and re-deletes `guest` |
| Queue missing or backlog growing | `queue_present` / `queue_backlog` | `runuser -u rabbitmq -- rabbitmqctl list_queues name messages` | Backlog → worker not consuming: follow the worker row; queue name must match `AUSTRO_RABBITMQ_QUEUE` (default `austro.events`) |

## Proxy / TLS

| Symptom | Signal | Try | If still broken |
|---|---|---|---|
| nginx disabled, 80/443 refused | `proxy_https` | The proxy is **fail-closed by design**: see §5 of free-native-deployment.md — place `/etc/austro/tls/fullchain.pem` + `privkey.pem`, `nginx -t`, `systemctl enable --now nginx` | This is not a defect; healthcheck reports `BLOCKED` for the proxy while TLS material is absent |
| `nginx -t` fails | `proxy_https` | Read the exact directive from the error; the shipped conf is self-contained | Confirm only the shipped `deploy/native/nginx.conf` is installed as `/etc/nginx/nginx.conf` (no fragments overriding it) |
| Cert renewed but still serving old | `proxy_https` | `nginx -s reload`; verify with `curl -fsSI https://<host>/` | Renewal is operator-managed; re-check the file mtimes in `/etc/austro/tls` |

## Backups and restores

| Symptom | Signal | Try | If still broken |
|---|---|---|---|
| `backup_fresh` red though a cron runs | `backup_fresh` | Check the cron output / exit code of `deploy/native/backup.sh` | Run it manually exactly as cron does (root) and read the first failing line |
| Restore aborts before confirmation | — (restore has no signal; it is the operator session) | Re-read `deploy/native/restore.sh`: it is destructive and requires `AUSTRO_RESTORE_CONFIRM=yes` (or typing the DB name) | If you are seeing this, it is working as intended — nothing was touched yet |
| Restore fails the post-restore verification | — | The restore stopped before reporting success — follow the exact invariant in the log line (tables, extension, RLS, roles) | Do **not** restart the worker/API stack "to finish"; re-verify each checked invariant from the restore script's list |

## Monitoring

| Symptom | Signal | Try | If still broken |
|---|---|---|---|
| Webhook alerts stop arriving | `alert_delivered` | Force an alert; inspect `/tmp/austro-monitor/last-alert` and the debounce interval | Confirm outbound egress from the VM to the webhook endpoint |

---

## Rules of engagement

- Read before acting: `journalctl -u <unit> -n 3000 --no-pager` first, always.
- Re-provisioning is safe: `bootstrap-ubuntu24.sh` is idempotent and does not
  touch data, but re-run it only after the env file exists.
- Never weaken a control to make a check pass: no `trust` auth, no exposed
  datastore ports, no `BYPASSRLS`, no disabling RLS, no
  dropping `AUSTRO_POSTGRES_RUNTIME_DSN`.
- If a mitigation is a one-off `systemctl restart`, it is a patch; log the root
  cause. If the change is durable, it belongs in `deploy/native/` first and the
  docs second.