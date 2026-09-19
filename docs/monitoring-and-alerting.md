# AUSTRO OS — Monitoring and Alerting

`scripts/monitor.sh` is the periodic operational monitor. It watches the running
production stack and emits one JSON line per signal, exits non-zero when any
signal is failing, and can push a failure alert to an operator webhook.

It is the **companion** to `scripts/healthcheck.sh`, not a replacement:

| Tool | Runs | Questions answered |
|---|---|---|
| `scripts/healthcheck.sh` | On every deploy, and on demand | Is the topology correct and secure right now? (deep: RLS, runtime role, credentials) |
| `scripts/monitor.sh` | Every minute, indefinitely | Is the stack healthy *over time*? (signals, drift, backlog, backup age, alerts) |

The healthcheck is the right tool when something happened and you need to know
*what is broken*. The monitor is the right tool when nothing has happened yet
and you need to be told *when it starts to break*.

---

## When to run it

Run it on a fixed schedule and treat a non-zero exit as the trigger for paging,
not as something a human runs by hand:

```bash
# every minute (cron)
* * * * *  cd /opt/austro-os && scripts/monitor.sh >>/var/log/austro-monitor.jsonl 2>&1 \
             || scripts/notify-incident.sh   # your paging glue

# systemd timer (unit: austro-monitor.timer)
[Unit]
Description=Run the AUSTRO OS operational monitor every minute
[Timer]
OnBootSec=30s
OnUnitActiveSec=1min
AccuracySec=5s
[Install]
WantedBy=timers.target
```

A systemd service unit that runs `scripts/monitor.sh` under the same user that
owns the compose project, with `Restart=on-failure` and
`StandardOutput=append:/var/log/austro-monitor.jsonl`, is a complete monitoring
setup — no separate agent is required by this repository.

---

## Signals

Every run emits a JSON line per signal
(`{"signal":"...","status":"pass|fail|info","detail":"..."}`) followed by a
`summary` line. One example line:

```json
{"signal":"api_ready","status":"pass","detail":"GET /health/ready answered ready"}
```

| Signal | What is checked | Fails when |
|---|---|---|
| `containers` | `postgres`, `redis`, `rabbitmq`, `api`, `worker` (plus the proxy unless skipped) are running | any required container is missing or not running |
| `api_live` | `GET /health/live` answered `"status":"ok"` | the API is not alive |
| `api_ready` | `GET /health/ready` answered `"ready":true` | the API is not ready to serve |
| `database_ready` | `pg_isready` against the owner role | PostgreSQL is not accepting connections |
| `database_runtime` | a real `SELECT 1` as the runtime role (`austro_app`) | the serving credential no longer works |
| `redis_auth` | an authenticated `PING` (`REDISCLI_AUTH`, no password on the process list) | Redis is down or the credential is wrong |
| `rabbitmq_up` | `rabbitmqctl status` | the broker is not reporting status |
| `queue_present` | the expected queue (`austro.events`) exists | the queue is missing |
| `queue_backlog` | queue depth vs `AUSTRO_QUEUE_BACKLOG_THRESHOLD` | depth exceeds the threshold (worker not draining) |
| `worker_attached` | the worker log shows `worker-started` | the worker never attached to the queue |
| `worker_errors` | error-level lines in the worker's latest 2000 log lines | the count exceeds `AUSTRO_MONITOR_WORKER_ERROR_LIMIT` (default: report only, never fail) |
| `backup_fresh` | age of the newest `backups/austro-postgres-*.sql.gz` | no backup exists, or it is older than `AUSTRO_BACKUP_MAX_AGE_HOURS` |
| `proxy_https` | a real HTTPS request through the proxy (`curl --resolve` to the local listener, like the healthcheck) | the proxy does not serve `/health/live` with HTTP 200 |
| `alert_delivered` | outcome of the webhook attempt, only emitted when a signal failed | n/a — delivery problems are reported, never a failing signal |
| `summary` | pass/fail counts and the process verdict | n/a — the verdict line |

Signals that cannot be evaluated (for example `proxy_https` when `curl` is
absent, or a signal skipped by configuration) are reported as `info` — never as
`pass`, and never as `fail`. The monitor never lies about having checked
something.

**Exit status:** `0` when no signal failed, `1` when at least one signal
failed. Alert delivery never changes the exit status.

---

## Thresholds

All thresholds read the same defaults the script itself uses. Unset means the
same thing to the script, this document, and `.env.example`.

| Variable | Default | Purpose |
|---|---|---|
| `AUSTRO_QUEUE_BACKLOG_THRESHOLD` | `100` | queue depth at which `queue_backlog` fails |
| `AUSTRO_MONITOR_WORKER_ERROR_LIMIT` | `0` | error-line budget in the worker window; `0` = reported, never failing |
| `AUSTRO_BACKUP_MAX_AGE_HOURS` | `24` | maximum acceptable age of the newest local backup |
| `AUSTRO_MONITOR_SKIP_PROXY` | `0` | `1` skips the `proxy_https` signal (CI runs without a proxy) |
| `AUSTRO_ALERT_WEBHOOK_URL` | unset | webhook that receives failure alerts |
| `AUSTRO_ALERT_MIN_INTERVAL_SECONDS` | `3600` | minimum gap between alerts for a sustained failure |
| `AUSTRO_ALERT_STATE_DIR` | `/tmp/austro-monitor` | directory holding the `last-alert` debounce state |

The defaults are deliberately conservative so the monitor reports truthfully
out of the box. Tune `AUSTRO_QUEUE_BACKLOG_THRESHOLD` to the observed steady
state of the workload; tune the alert interval to your on-call cadence.

---

## Alerting contract

When at least one signal failed, `AUSTRO_ALERT_WEBHOOK_URL` is set, and `curl`
is available, the monitor POSTs a single JSON document:

```json
{"system":"austro-os","status":"degraded","failed":" api_live queue_backlog","timestamp":1777550000}
```

- `failed` lists the failing signal names (space-separated).
- Delivery is **debounced through a state file**: after a successful delivery,
  the epoch is written to `$AUSTRO_ALERT_STATE_DIR/last-alert`, and no new alert
  is sent until `AUSTRO_ALERT_MIN_INTERVAL_SECONDS` have elapsed. A sustained
  failure pages once, not every minute; the run page happens on transition.
- A delivery failure is reported as an `alert_delivered` info line and retried
  after the next debounce window. It is never a failing signal itself.
- The request body contains no credentials, no connection strings and no tracing
  payloads — only the signal list above. Nothing the monitor reads (including
  `AUSTRO_POSTGRES_RUNTIME_PASSWORD`) is ever written to a log line or the
  webhook.

Any small HTTP endpoint that accepts a POST (a messaging-hook receiver, a
flask/express route, a serverless function) can consume this contract. The
repository ships no receiver: choosing one is operator configuration.

### Proving the alert path works

The repo does not ship a webhook receiver, so the alert branch itself cannot be
exercised in CI. It can be proven on a staging host:

```bash
# terminal 1 — a throwaway receiver
nc -l -p 9000 >/tmp/alert-body.json    # prints what a webhook receives

# terminal 2 — force a clean, safe failure and point the monitor at the receiver
AUSTRO_BACKUP_MAX_AGE_HOURS=0 \
AUSTRO_ALERT_WEBHOOK_URL=http://127.0.0.1:9000/austro-alerts \
scripts/monitor.sh ; echo "exit=$?"
```

Expect `MONITOR: DEGRADED`, a non-zero exit, and `{"system":"austro-os",...}`
printed by the receiver. Setting `AUSTRO_BACKUP_MAX_AGE_HOURS=0` fails only the
backup-freshness signal and touches nothing else — a safe way to prove end to
end that failures reach the operator.

---

## Operator configuration checklist

For a production deployment, in addition to the variables in
[`production-configuration.md`](production-configuration.md):

1. Choose a webhook receiver and set `AUSTRO_ALERT_WEBHOOK_URL`.
2. Run `scripts/monitor.sh` from cron or a systemd timer every minute.
3. Point the logs (`/var/log/austro-monitor.jsonl`) at your log shipping
   destination so historical signal lines survive host restarts.
4. Confirm a failed run actually pages someone (see the proof procedure above).
5. Confirm `scripts/backup.sh` also runs on a schedule (see
   [`backups-and-restore.md`](backups-and-restore.md)) — `backup_fresh` is only
   meaningful if backups are being created.
6. Review `AUSTRO_QUEUE_BACKLOG_THRESHOLD` against the workload's steady-state
   queue depth before relying on the backlog signal.

---

## What this does not cover

- **Inside the containers.** The monitor reads the stack through `docker
  compose`; it does not enter containers or read application metrics beyond the
  API's health endpoints and the process logs. Per-process metrics (Go runtime,
  HTTP latency histograms) are not exported by the application today.
- **Retention and history.** The monitor emits lines; storage, retention and
  dashboards are the operator's logging stack, not part of the repository.
- **Off-host backup freshness.** `backup_fresh` checks the local artifact only.
  Off-host copies are `scripts/backup.sh`'s S3 path and are verified by that
  script, not by the monitor.
- **Load.** The monitor tells you the stack is healthy, not that it is fast.
  Performance baselines are a separate exercise (see the production readiness
  certification).