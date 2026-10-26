# AUSTRO OS — AlwaysData Native Deployment Runbook

This runbook describes the native deployment of AUSTRO OS to an AlwaysData
managed hosting account. Unlike the Ubuntu ARM64 native deployment (which runs
everything on a single VM), this deployment uses AlwaysData managed
PostgreSQL and RabbitMQ, runs the AUSTRO API as an AlwaysData **User Program
site**, and runs the dispatcher and the RabbitMQ worker as AlwaysData
**Services**.

> **Status: architecture documented, target account not yet validated.**
> Everything in "Platform capabilities" below is taken from AlwaysData's own
> documentation and the links are given. Everything in "Account checks required
> before deploying" is *not* verifiable from this repository and must be
> confirmed on the real account first. Do not treat this runbook as a record of
> a successful deployment.

## Platform capabilities (official documentation)

These are the platform facts the topology below depends on. Each was read from
the linked AlwaysData page.

| Capability | Fact | Source |
|------------|------|--------|
| Services are supervised | A Service is a long-running custom program; the system restarts it when it stops | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| Services require a paid plan | Available on all paid plans, from the **Small** plan up. The Free plan cannot run Services and may not be used for a commercial deployment | [Plan comparison](https://help.alwaysdata.com/en/docs/admin-billing/billing/choose-its-paas), [Public Cloud prices](https://help.alwaysdata.com/en/docs/admin-billing/billing/public-cloud-prices) |
| Restart signal | Restarting a Service sends `SIGHUP` to the process | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| Repeated failure is fatal to the Service | A Service that fails repeatedly in a short period is **automatically disabled** by the platform and stops being restarted | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| No inbound filtering | "No network filtering is available" — any host able to reach a Service port can connect to it | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| Service ports | `8300-8499`, published on `services-[account].alwaysdata.net`; bind `[::]` to accept IPv6 | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| Service isolation (Public Cloud) | Services run on servers **separate** from the HTTP and SSH servers, and are reachable over **IPv6 only**. There is no IPv4-only fallback for a Service port | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| Service isolation (Private Cloud) | The `8300-8499` ports are **not** exposed to the internet; a Service may use other ports | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| Optional monitor | A Service may define a monitoring command; a non-zero exit triggers a restart | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| Service logs | `~/admin/logs/services/` on the account, and *Advanced → Processes → Services* in the panel | [Services](https://help.alwaysdata.com/en/docs/web-hosting/services) |
| Go sites | A Go site is a **User Program**; the application must listen on the IP and port the site displays, available as `$IP`/`$HOST` and `$PORT` | [Go configuration](https://help.alwaysdata.com/en/docs/web-hosting/languages/go/configuration) |
| Site logs | `~/admin/logs/sites/` | [User Program](https://help.alwaysdata.com/en/docs/web-hosting/sites/http-servers/user-program) |
| Scheduled Tasks are not consumers | A Task that starts while the previous run is unfinished is **skipped**, and Tasks are cron-scheduled | [Tasks](https://help.alwaysdata.com/en/docs/web-hosting/tasks) |
| Managed RabbitMQ | `rabbitmq-[account].alwaysdata.net:5672`; the management plugin is **Private Cloud only** | [RabbitMQ](https://help.alwaysdata.com/en/docs/web-hosting/databases/rabbitmq) |
| Managed Redis | **Private Cloud only.** Not offered on Public Cloud | [Plan comparison](https://help.alwaysdata.com/en/docs/admin-billing/billing/choose-its-paas) |
| Resource limits UI | The per-account disk/CPU/RAM limit panel is **Private Cloud only** | [Resource limits](https://help.alwaysdata.com/en/docs/technical-specifications/system-resources-alerts-and-limitations/) |

Two of these decide the whole topology:

- **A Task cannot replace a Service.** Tasks skip overlapping runs, so a Task
  is not a durable consumer. A long-lived process that must stay up needs a
  Service.
- **A Service port is public.** Anything reachable on `8300-8499` is reachable
  from the internet, so the API — which must sit behind HTTPS — must not be a
  Service. It is a User Program site instead, which is what the platform
  documents for Go.

## Architecture

```
                              Internet
                                   |
                          443     v   (TLS terminated by the site)
                    +-----------------------------+
                    | AlwaysData site             |  type: User Program
                    |  austro-api                 |  ~/austro/current/austro-api
                    |  listens on the site's      |  ROLE=api,  MODE=off
                    |  $IP:$PORT                  |  no sweep loop here
                    |  + Redis (memory bank)      |  /health/ready checks the
                    +--------------+--------------+  dispatcher's heartbeat
                                   |
                                   v
  +---------------------------------------------------+
  | AlwaysData Services  (services-[account], 8300+)  |
  |                                                   |
  |  +-------------------+       +-----------------+  |
  |  | austro-dispatcher  |       | austro-worker   |  |
  |  | austro-api binary, |       | austro-worker   |  |
  |  | ROLE=dispatcher    |       | binary,         |  |
  |  | MODE=service       |       | ROLE=worker     |  |
  |  | sweep loop only,   |       | MODE=off        |  |
  |  | no listener,       |       | consumes the    |  |
  |  | no port, no Redis  |       | queue only      |  |
  |  +---------+-----------+       +--------+--------+  |
  |            |                            |           |
  |            | sweeps claimed retries     | AMQP      |
  |            v                            | consume   |
  |   +--------------------+      +-------------------+  |
  |   | PostgreSQL         |      | RabbitMQ          |  |
  |   | (AlwaysData)       |      | (AlwaysData)      |  |
  |   | executions, leases,|      | austro.executions  |  |
  |   | process_heartbeats |      | austro.events      |  |
  |   +--------------------+      +-------------------+  |
  +---------------------------------------------------+

  No Service in this diagram holds a port. Neither the dispatcher nor the worker
  opens a listener, so "no network filtering on 8300-8499" has nothing to bite
  on. That is the property the split is built around, not a happy accident.
```

**The three units, and what each one is declared to be:**

| Unit | Binary | `AUSTRO_PROCESS_ROLE` | `AUSTRO_EXECUTION_DISPATCH_MODE` | Port | Redis |
|------|--------|-----------------------|----------------------------------|------|-------|
| `austro-api` (site) | `austro-api` | `api` | `off` | site `$IP:$PORT` | yes |
| `austro-dispatcher` (Service) | `austro-api` | `dispatcher` | `service` | **none** | no |
| `austro-worker` (Service) | `austro-worker` | `worker` | `off` | **none** | no |

Every other role/mode pairing is refused at startup. `api` + `service` would serve
HTTP from the process whose whole purpose is to hold no port; `dispatcher` + `api`
is the same mistake from the other side; `worker` + `api`/`service` gives the
retry queue two sweep owners. The refusals are the mechanism, not a lint: with two
sweepers running, both claim the same due retries, and while the SQL claim and
lease fencing in `internal/execution` bounds the damage, it does not make two
sweepers correct.

The role is not decorative. `austro-api` refuses to start on `role=worker` and
`austro-worker` refuses to start on any other role, so a shared environment file
that labels the units wrongly fails loudly instead of quietly opening a listener
where one was not wanted.

**Why the API cannot be a Service:** AlwaysData Service ports are unfiltered and,
on Public Cloud, IPv6-only. Publishing the API there would expose it in plaintext
and bypass the platform's TLS termination. It is a User Program site instead,
which is what the platform documents for Go. A Scheduled Task is not an
alternative for either long-running unit: overlapping runs are skipped, so a Task
is not a durable consumer.

## How a missing dispatcher becomes visible

This is the one failure the split introduces, and it is worth stating plainly
because it is silent by nature.

Before the split, the API process swept retries itself, so "dispatch is working"
and "the API is running" were the same fact. After the split they are different
processes on different hosts, and the API cannot see the dispatcher at all. If the
dispatcher Service is stopped, crash-looping, or was never created, the API keeps
serving traffic, keeps returning `200` on a naive readiness check, and executions
pile up in `pending` state in PostgreSQL. Nothing reports it.

Two mechanisms close that, and they are deliberately separate:

1. **A declaration, enforced at startup.** The site sets
   `AUSTRO_EXPECT_COMPANION_PROCESSES=dispatcher`. A production API on a
   broker-backed transport that does not sweep and has not named a dispatcher is a
   startup failure. This is a statement about the deployment's shape — a process
   genuinely cannot observe a companion on another host, so there is nothing to
   measure here. Add `worker` to the list if the queue is actually being drained;
   leave it off otherwise, so an API that admits work before its first worker
   exists is not taken out of rotation.

2. **An observation, reported on every readiness check.** The dispatcher and the
   worker each write a heartbeat row to `process_heartbeats`
   (`role`, `instance_id`, `last_seen_at`). The site's `/health/ready` reads them
   and returns **`503`** with `"reason":"companion-absent"` when a required
   companion has not reported within `AUSTRO_HEARTBEAT_STALE_AFTER` (default
   60s). Freshness is computed by PostgreSQL's own clock, so a skew between hosts
   cannot make a running companion read as dead.

A declared companion is never treated as a healthy one. The declaration gets the
process to start; the heartbeat is what tells you it is actually there:

```bash
# What the site believes it needs, and what it can actually see.
curl -s https://<site-domain>/health/ready | python3 -m json.tool
```

```json
{
  "ready": false,
  "status": "degraded",
  "reason": "companion-absent",
  "requires": ["dispatcher"],
  "companions": [
    { "role": "dispatcher", "instances": 0 }
  ]
}
```

The `"ready"` field stays first and keeps its boolean shape, so the existing
scripts that match on the body keep working; the status code is what carries the
change to a platform supervisor that only reads the code. A single-process
deployment declares no companions, answers `200`, and is unaffected.

**Direct check, independent of the site:**

```sql
SELECT role, instance_id, last_seen_at,
       now() - last_seen_at AS age
FROM process_heartbeats
ORDER BY role, last_seen_at DESC;
```

A row whose `age` exceeds `AUSTRO_HEARTBEAT_STALE_AFTER` is not evidence of
anything — the process behind it is gone, and the site's readiness check is
already reporting `503` because of it.

**A caveat worth stating.** The heartbeat proves a process is *running and able to
reach PostgreSQL*. It does not prove the dispatcher is claiming anything, nor that
the worker is draining. `AUSTRO_EXECUTION_WORKSPACE_IDS` being empty or wrong on
the dispatcher is still a silent no-op, and the only way to catch that is to admit
a real execution and watch it move:

```sql
SELECT status, count(*) FROM ai_executions GROUP BY status ORDER BY status;
```

## Key Differences from Ubuntu Native Deployment

| Aspect | Ubuntu Native | AlwaysData |
|--------|--------------|------------|
| PostgreSQL | Self-hosted (systemd) | AlwaysData managed |
| RabbitMQ | Self-hosted (systemd) | AlwaysData managed |
| Redis | Self-hosted (systemd), used by all processes | **Unresolved — see below.** Used by the API only |
| TLS | Self-hosted (fail-closed) | Terminated by the User Program site |
| Process Manager | systemd | AlwaysData Service Manager |
| User | austro (OS user) | [account] (AlwaysData SSH user) |
| Release Dir | /opt/austro/releases | ~/austro/releases |
| Symlink | /opt/austro/current | ~/austro/current |
| API listener | 127.0.0.1:8080 (loopback only) | the site's `$IP:$PORT` |
| Dispatch loop | inside the API process | its own Service, no port |
| Redis reachable by | API, dispatcher, worker | **API only** |
| Missing-dependency detection | same process, so none needed | heartbeat table + 503 on `/health/ready` |

## Unresolved: Redis

AlwaysData offers a managed Redis on **Private Cloud only**, and the API is the
only process that uses one. That narrows the blast radius: if this question is
answered badly, the API does not start and execution dispatch keeps working.

**First, a correction to the earlier version of this runbook:** the dispatcher and
the worker do not use Redis and never did open a connection. `redis.Initialize` is
called only on the API path, and the memory bank it backs is composed into the API
runtime only — `internal/composition` takes no memory store, so a worker composed
from the same function has nothing to dial. Production validation now requires
`AUSTRO_REDIS_ADDR` only for a process that actually connects, so the dispatcher
and the worker run on a host with no Redis at all.

That makes the options:

| Option | Where Redis runs | Exposure | Account needed | Verdict |
|--------|------------------|----------|----------------|---------|
| A. Managed AlwaysData Redis | Private Cloud, AlwaysData-operated | None beyond the account | **Private Cloud** | **Preferred** |
| B. External managed Redis | A third-party managed service | Authenticated TLS, IP-restricted | Any | Acceptable, with cost |
| C. Self-hosted Redis on a Service | AlwaysData Service, `8300-8499` | **Internet-reachable, unfiltered, IPv6-only** | Any | Needs an explicit written acceptance |
| D. Remove Redis, use PostgreSQL | Nowhere | None | None | **Not available** — see below |

**Why option C is not the default.** A Service port is reachable from the internet
with no network filtering, is IPv6-only on Public Cloud, and on Private Cloud the
`8300-8499` range is not exposed at all — so the same configuration fails in
opposite directions on the two clouds. A Redis holding workspace memory, with only
a password as protection, on a port anyone on the internet can reach, is a
materially different risk posture from the Ubuntu deployment. It is listed so the
option is visibly available, not because it is recommended. If it is chosen, record
the decision and the accepted risk in this file rather than leaving it implicit.

**Why option D is not available.** The memory bank is not a cache. `internal/memory`
implements four layers — session, long-term, workspace, and organizational memory
— with per-layer TTLs, and the API is the only writer. Moving it to PostgreSQL is
not a configuration change: it is a new persistence adapter against a port
currently satisfied by `redis.MemoryStore`, with its own concurrency semantics,
and it would put workspace-scoped writes on the same database the row level
security policies already constrain. It is a project, not a toggle, and it is not
available as a way out of this decision.

**The TLS gap that used to block options B and C is closed.** This runbook
previously recorded that the Redis client was "configured with address, password,
and database index only — there is no TLS setting", which meant an external
managed Redis reached over TLS could not be used at all. `AUSTRO_REDIS_ADDR` is
now the whole connection configuration and its form selects the transport:

| Value | Transport |
|---|---|
| `host:port` | plaintext (legacy; unchanged) |
| `redis://host:port` | plaintext, stated explicitly |
| `rediss://host:port` | TLS 1.2 or later, server certificate verified |

`AUSTRO_REDIS_CA_FILE` optionally supplies additional certificate authorities for
a private CA and is added to the system roots rather than replacing them.
Certificate verification cannot be turned off by any setting: `InsecureSkipVerify`
is set by no input and reachable from no configuration.

So option B is no longer blocked by the client. Two things about it remain
unproven and are not claimed here: **no live TLS Redis endpoint was available**, so
what is tested is that a `rediss://` address produces a client that verifies a
certificate issued by the supplied CA and rejects one that is not — verified
offline against a generated authority, not against a real service. And option A
still depends on the account being Private Cloud, which is an account fact, not a
code question.

For option A, set `AUSTRO_REDIS_ADDR=rediss://<alwaysdata-redis-host>:<port>`.
Whether AlwaysData's managed Redis presents a publicly-issued certificate or one
from a private authority is a panel fact to confirm on the account; if it is a
private authority, export its PEM and point `AUSTRO_REDIS_CA_FILE` at it.

## Account checks required before deploying

None of these can be answered from this repository. Confirm them on the account:

- [ ] The plan is **Small or above**. A Free plan cannot run Services at all.
- [ ] Is the account Public Cloud or Private Cloud? The answer decides the
      Redis question (option A requires Private Cloud), whether Service ports are
      IPv4-reachable at all, and whether resource limits are visible.
- [ ] Services are enabled and can be created in the panel.
- [ ] A User Program site can be created and the API can bind the `$IP:$PORT`
      the panel displays.
- [ ] Outbound connectivity works **from both** the site process and the Service
      processes to `postgresql-[account].alwaysdata.net:5432` and
      `rabbitmq-[account].alwaysdata.net:5672`. Services run on different hosts
      than the site, so one working does not imply the other.
- [ ] **The site and the dispatcher can both reach PostgreSQL.** This is the
      heartbeat path, and a site that cannot reach the database gets
      `liveness-unavailable` on `/health/ready` rather than a truthful answer.
- [ ] PostgreSQL TLS (`sslmode=require`) is accepted from those hosts.
- [ ] `~/admin/logs/services/` is readable and both services show as running
      under *Advanced → Processes → Services*.
- [ ] Neither service has been auto-disabled after repeated failures — the
      platform stops restarting a Service that fails repeatedly, which is
      indistinguishable from a dispatcher that was never created until you look
      at the panel.
- [ ] The plan's RAM and CPU ceiling can carry the API, the dispatcher, and the
      worker concurrently. This is not measurable in advance on Public Cloud,
      where the limits panel is not available.
- [ ] **The Redis decision above is recorded**, with the option chosen and, for
      option C, the accepted exposure in writing.
- [ ] `/health/ready` returns `200` once all three units are up, and `503` with
      `companion-absent` after the dispatcher Service is stopped. Verify the
      second half too — a readiness check that has only ever been seen green has
      not been tested.


## Directory Layout

```
~/
├── austro/
│   ├── current -> releases/<sha>
│   ├── releases/
│   │   └── <sha>/
│   │       ├── austro-api
│   │       └── austro-worker
│   ├── austro.env          # Production environment file (from austro.env.alwaysdata)
│   └── deploy/             # Deployment scripts (optional, for manual ops)
└── admin/logs/{sites,services}/
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
   sftp [account]@ssh-[account].alwaysdata.net
   put austro-api
   put austro-worker
   ```

3. **Install the release** on AlwaysData:
   ```bash
   ssh [account]@ssh-[account].alwaysdata.net
   ~/austro/deploy/alwaysdata/install-release.sh \
     ~/austro/releases/<sha>/austro-api \
     ~/austro/releases/<sha>/austro-worker \
     <sha>
   ```

   The install script will:
   - Verify binary checksums
   - Create the release directory
   - Atomically switch the `~/austro/current` symlink
   - Restart the API site and both Services

4. **Verify the deployment**:
   ```bash
   ~/austro/deploy/alwaysdata/verify-alwaysdata.sh
   ```

## Environment Configuration

The production environment file is `~/austro/austro.env` (copied from
`deploy/alwaysdata/austro.env.alwaysdata` and filled with real secrets).

The file is the same for the site and for both Services, except for
`AUSTRO_EXECUTION_DISPATCH_MODE` and `AUSTRO_SERVER_ADDR`. If the env file sets
`AUSTRO_SERVER_ADDR`, and the panel offers no per-process override for it, then
give each of the three units its own copy of the file and set the two variables
that differ. AlwaysData Services read the environment from the Service
definition, and the site reads it from the site definition; the panel entry is
what wins.

Key settings for AlwaysData:

| Variable | Value | Notes |
|----------|-------|-------|
| `AUSTRO_ENV` | `production` | |
| `AUSTRO_EXECUTION_DISPATCH_MODE` | `off` on the site, `service` on the dispatcher | Which process sweeps. `off` on the site is valid in production only with a declared companion; paired with the wrong role it is a startup failure. |
| `AUSTRO_SERVER_ADDR` | the site's `$IP:$PORT` | Site only. Ignored by the dispatcher and worker, which open no listener. |
| `AUSTRO_POSTGRES_PREPROVISIONED_ROLES` | `true` | **Required** for AlwaysData |
| `AUSTRO_POSTGRES_DSN` | `postgres://austro:...@postgresql-[account].alwaysdata.net:5432/austro_os?sslmode=require` | Owner DSN |
| `AUSTRO_POSTGRES_RUNTIME_DSN` | `postgres://austro_app:...@postgresql-[account].alwaysdata.net:5432/austro_os?sslmode=require` | Runtime DSN |
| `AUSTRO_POSTGRES_ADMIN_DSN` | `postgres://austro_app_admin:...@postgresql-[account].alwaysdata.net:5432/austro_os?sslmode=require` | Admin DSN |
| `AUSTRO_REDIS_ADDR` | managed Redis, or the Service address. Use `rediss://host:port` when the service requires TLS. **API site only** | **Choice still open — see "Unresolved: Redis"; TLS is no longer a blocker** |
| `AUSTRO_REDIS_CA_FILE` | PEM bundle, only if the Redis certificate comes from a private CA | **API site only** |
| `AUSTRO_RABBITMQ_URL` | `amqp://austro:...@rabbitmq-[account].alwaysdata.net:5672/[vhost]` | vhost `[vhost]` |
| `AUSTRO_PROCESS_ROLE` | `api` / `dispatcher` / `worker` | **Per unit.** Both binaries refuse a role they cannot serve |
| `AUSTRO_EXECUTION_TRANSPORT_BACKEND` | `rabbitmq` | `stub` is refused in production unless the exemption is set |
| `AUSTRO_EXECUTION_DISPATCH_MODE` | `off` (site), `service` (dispatcher), `off` (worker) | **Per unit.** Must agree with the role or startup is refused |
| `AUSTRO_EXPECT_COMPANION_PROCESSES` | `dispatcher` (site) | Required on a production site that does not sweep |
| `AUSTRO_HEARTBEAT_INTERVAL` | `15s` | Optional; safe default shown |
| `AUSTRO_HEARTBEAT_STALE_AFTER` | `60s` | Optional; must stay strictly above the interval |
| `AUSTRO_EXECUTION_QUEUE` | `austro.executions` | Must match between dispatcher and worker |
| `AUSTRO_EXECUTION_WORKSPACE_IDS` | the workspace UUIDs served | Required on the **dispatcher only**; production refuses a non-sweeper that has one |

## Service Management

AlwaysData uses its own service manager, not systemd. Units are created in the
AlwaysData web panel.

**Each unit needs its own environment file.** The role and dispatch mode differ
per unit, and both binaries now refuse a role they cannot serve, so a single
shared file cannot be used. Copy `austro.env.alwaysdata` three times and vary only
`AUSTRO_PROCESS_ROLE`, `AUSTRO_EXECUTION_DISPATCH_MODE`,
`AUSTRO_EXPECT_COMPANION_PROCESSES`, `AUSTRO_EXECUTION_WORKSPACE_IDS`,
`AUSTRO_SERVER_ADDR`, and the Redis pair.

### Site: `austro-api` (User Program)

- **Type**: User Program
- **Command**: `~/austro/current/austro-api`
- **Environment**: env file with `AUSTRO_PROCESS_ROLE=api`,
  `AUSTRO_EXECUTION_DISPATCH_MODE=off`,
  `AUSTRO_EXPECT_COMPANION_PROCESSES=dispatcher`, Redis pair **present**,
  `AUSTRO_EXECUTION_WORKSPACE_IDS` **absent**
- **Address**: the IP and port the panel displays; the process must bind it
- **Auto-restart**: managed by the platform for sites

### Service: `austro-dispatcher`

- **Type**: Service
- **Command**: `~/austro/current/austro-api`
- **Working Directory**: `~/austro/current`
- **Environment**: env file with `AUSTRO_PROCESS_ROLE=dispatcher`,
  `AUSTRO_EXECUTION_DISPATCH_MODE=service`,
  `AUSTRO_EXECUTION_WORKSPACE_IDS` **set**, Redis pair **absent**
- **Port**: none — the process opens no listener
- **Instances**: exactly one. A second instance would sweep the same retries.
- **Monitoring command**: leave unset (see below)
- **Logs**: `~/admin/logs/services/`

### Service: `austro-worker`

- **Type**: Service
- **Command**: `~/austro/current/austro-worker`
- **Working Directory**: `~/austro/current`
- **Environment**: env file with `AUSTRO_PROCESS_ROLE=worker`,
  `AUSTRO_EXECUTION_DISPATCH_MODE=off`, Redis pair **absent**
- **Port**: none
- **Monitoring command**: leave unset (see below)
- **Logs**: `~/admin/logs/services/`

### Monitoring commands: leave them unset

A Service may run a monitoring command and be restarted when it exits non-zero.
On this platform that is a trap: the restart signal is `SIGHUP`, and a monitor
that flaps turns a momentary blip into a restart loop — after which the
platform **disables the Service** and it stops coming back on its own. A
monitoring command that is merely "a bit noisy" is worse than none. Use the
platform's own status and the logs, and add external monitoring on queue depth
and worker progress instead.

If a monitoring command is ever configured, it must exit non-zero only for a
condition that a restart genuinely fixes, and must be stable enough to survive
a slow dependency without flapping.

The heartbeat table is not a substitute for it either. A heartbeat that has gone
stale says the process is gone; it does not say a restart would help, which is
the only question a monitoring command is allowed to answer.

## Database Bootstrap

The application performs its own schema bootstrap on startup using the
owner DSN (`AUSTRO_POSTGRES_DSN`). In pre-provisioned mode:

1. The owner role `austro` creates extensions (`vector`, `pgcrypto`)
2. Creates/updates schema (tables, indexes, RLS policies)
3. Verifies pre-provisioned roles exist with correct attributes
4. Grants permissions to runtime and admin roles
5. Switches to runtime DSN for serving traffic

The bootstrap is idempotent and runs on every API startup. Only the site runs
the full serving bootstrap; the dispatcher and worker connect as configured.

`process_heartbeats` is created by the same bootstrap, and is deliberately
**not** covered by row level security: "is this process alive" is infrastructure
state with no workspace owner, and the API must be able to read the dispatcher's
row regardless of which workspace is being served. It carries no tenant content —
a role name, an opaque instance id, and a timestamp. The existing
`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES` covers it for the runtime
role, so no additional grant is needed.

## Health Checks

- **Liveness**: `GET /health/live` on the site — the API process is alive.
  Deliberately unconditional: it answers "is this process running", and a
  liveness check that fails on a slow dependency gets the process killed.
- **Readiness**: `GET /health/ready` on the site — can the **deployment** serve.
  Returns `503` when a required companion is not reporting.

> **Readiness is no longer a stub, and it is no longer unconditional.** It reads
> the companion heartbeats and reports what it observes. A production site that
> does not sweep must declare its dispatcher with
> `AUSTRO_EXPECT_COMPANION_PROCESSES`, and a required companion that has not
> reported within `AUSTRO_HEARTBEAT_STALE_AFTER` produces
> `503 {"ready":false,"reason":"companion-absent",...}`.
>
> It still does not probe PostgreSQL, Redis, or RabbitMQ directly. It does read
> PostgreSQL to read the heartbeats, so a database the site cannot reach shows up
> as `liveness-unavailable` rather than as a healthy deployment.
>
> What it does **not** prove: that the dispatcher is claiming anything. An empty
> or wrong `AUSTRO_EXECUTION_WORKSPACE_IDS` on the dispatcher still produces a
> process that beats on schedule and dispatches nothing. See "How a missing
> dispatcher becomes visible" for the query that catches that.

> **A single-process deployment is unaffected.** With no companions declared,
> `/health/ready` answers `200` and `"requires":[]`, exactly as before. This is
> asserted by a test rather than assumed.

## Rollback Procedure

If a release causes issues:

1. SSH to AlwaysData
2. Switch the symlink back to the previous release:
   ```bash
   ln -sfn ~/austro/releases/<previous-sha> ~/austro/current
   ```
3. Restart the site and both Services from the panel. The platform restarts a
   Service by sending `SIGHUP`; both binaries handle it as a graceful shutdown,
   so in-flight HTTP requests and unacknowledged broker messages are settled
   before exit rather than dropped.

> **Rolling back across the split is a configuration change, not just a symlink.**
> The role-aware settings (`AUSTRO_PROCESS_ROLE`,
> `AUSTRO_EXPECT_COMPANION_PROCESSES`, the `503` readiness response) are part of
> this release. A release from before it will not read them, and one from after
> it will refuse to start without them, so roll back the three env files
> together with the binaries. A mixed state fails at startup rather than running
> half-split, which is the intended behaviour — but it means the env files are
> part of the release, not part of the account.

The `install-release.sh` script keeps the previous release for instant rollback.

## Troubleshooting

| Issue | Check |
|-------|-------|
| API site won't start | Check the env file for correct DSNs and passwords; confirm the process bound the `$IP:$PORT` the panel shows |
| Site serves 503 on `/` | The site is up but the process is not listening on the address AlwaysData expects |
| `/health/ready` returns 503 `companion-absent` | Working as designed: a required companion is not reporting. Check the Service is running and not auto-disabled, then `SELECT * FROM process_heartbeats` |
| `/health/ready` returns 503 `liveness-unavailable` | The site cannot read the heartbeat table. Almost always PostgreSQL reachability or the schema not yet bootstrapped — a different problem from a missing dispatcher, and deliberately reported differently |
| `austro-api` refuses to start with "serves the api and dispatcher roles" | `AUSTRO_PROCESS_ROLE=worker` is set on the site. It should be `api` |
| `austro-worker` refuses to start with "this binary is the worker" | The worker's env file has a role other than `worker`, almost always a shared env file copied without changing the role |
| Every unit refuses to start with a dispatch-mode error | The role and mode disagree. See the matrix in `austro.env.alwaysdata`; `api`+`off`, `dispatcher`+`service`, and `worker`+`off` are the accepted pairs |
| Site refuses to start with `AUSTRO_EXPECT_COMPANION_PROCESSES` | A production site on the broker transport that does not sweep has not named its dispatcher. Add `dispatcher` to the list |
| DB connection failed | Verify PostgreSQL TLS, credentials, and that the `austro_app` role exists |
| Redis connection failed | Check `AUSTRO_REDIS_ADDR`; if it is a Service, check that the account is Public Cloud and that the client can reach it over IPv6. Note this only applies to the **site** — the dispatcher and worker never dial Redis and do not have this setting |
| RabbitMQ connection failed | Verify vhost `[vhost]`, user `[user]`, port 5672, and that this host is allowed to reach the broker |
| Service disabled in the panel | Repeated failures. Find the crash in `~/admin/logs/services/` and fix the cause before re-enabling — restarting into the same condition disables it again. The heartbeat for that role will be absent, which is what the site's 503 is reporting |
| Service exits right after start | Almost always the env file: the dispatcher and worker are strict about their configuration and exit rather than run degraded. Read the message — it names the setting |
| Worker not processing | Check worker logs, then both queues: `austro.events` for pipeline/publication events and `austro.executions` (the `AUSTRO_EXECUTION_QUEUE` value) for AI execution dispatches. A worker that consumes only the first while the second backs up means the two processes disagree about the queue name — check `AUSTRO_EXECUTION_QUEUE` is set identically for the dispatcher and the worker. |
| Retries are claimed but never run | The dispatcher is not running. The site should be reporting `503 companion-absent`; if it is not, check `AUSTRO_EXPECT_COMPANION_PROCESSES` is actually set |
| Nothing runs, no heartbeat, no 503 | The dispatcher never started, and the site does not declare it as required. That combination is a configuration gap, not a bug — see "How a missing dispatcher becomes visible" |
| Readiness looks green but executions pile up | The dispatcher is beating but sweeping nothing. `AUSTRO_EXECUTION_WORKSPACE_IDS` is empty or wrong on the dispatcher Service. The heartbeat cannot detect this; check `SELECT status, count(*) FROM ai_executions GROUP BY status` |

## Security Notes

- All secrets in `~/austro/austro.env` (chmod 600)
- PostgreSQL uses TLS (`sslmode=require`)
- RabbitMQ uses a dedicated vhost `[vhost]` and user `[user]`
- The API is a User Program site, so TLS is terminated by the platform and the
  application never serves plaintext HTTP to a public port
- Neither the dispatcher nor the worker opens a listening port, so there is
  nothing to reach
- **Service ports are not private.** A Service bound to `8300-8499` is reachable
  from the internet with no network filtering. Anything put there must be safe
  to expose. This is why the API is not a Service, and why a self-hosted Redis
  on a Service port is flagged as unresolved above.

## Useful Commands

```bash
# API site logs
tail -f ~/admin/logs/sites/$(ls -t ~/admin/logs/sites/ | head -1)

# Service logs (both units)
ls -t ~/admin/logs/services/
tail -f ~/admin/logs/services/$(ls -t ~/admin/logs/services/ | head -1)

# Check the API against the site's own address.
# Note: /health/ready legitimately returns 503 on a split deployment whose
# dispatcher is not reporting, so -f will fail there by design. Use -sS and read
# the body instead of treating a non-zero exit as an outage.
curl -fsS "http://$IP:$PORT/health/live"
curl -sS  "http://$IP:$PORT/health/ready" | python3 -m json.tool

# Confirm the deployed binaries match the release you think is deployed
~/austro/current/austro-api    --version
~/austro/current/austro-worker --version

# Which process is bound to what. Exactly one listener is expected, and it
# belongs to the site. A second one means a Service is holding a port it was
# configured not to have.
ss -ltnp

# Process liveness, straight from the table rather than through the site.
# 'age' beyond AUSTRO_HEARTBEAT_STALE_AFTER means the row is not evidence of
# anything, and the site's readiness check is already reporting 503.
psql "postgres://austro_app:...@postgresql-[account].alwaysdata.net:5432/austro_os?sslmode=require" \
  -c "SELECT role, instance_id, now() - last_seen_at AS age FROM process_heartbeats ORDER BY role"

# Execution progress. The heartbeat cannot detect a dispatcher that is running
# but sweeping nothing, and this can.
psql "postgres://austro_app:...@postgresql-[account].alwaysdata.net:5432/austro_os?sslmode=require" \
  -c "SELECT status, count(*) FROM ai_executions GROUP BY status ORDER BY status"

# Test database connectivity
psql "postgres://austro_app:...@postgresql-[account].alwaysdata.net:5432/austro_os?sslmode=require" -c "SELECT 1"

# Test Redis (API site only — the other two units have no Redis setting)
redis-cli -h <host> -p <port> -a <password> PING

# Test RabbitMQ reachability
nc -vz rabbitmq-[account].alwaysdata.net 5672

# Prove the shutdown path is graceful rather than a kill: SIGHUP is what the
# platform sends. Watch for a shutdown log line, not just a PID change.
kill -HUP "$(pgrep -f austro-dispatcher)"
```
