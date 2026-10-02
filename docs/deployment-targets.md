# AUSTRO OS — Deployment Targets

Which deployment models this repository supports today, which it deliberately
does not, and what each requires from the host.

The authoritative baseline for this document is `main` at
`4a41a902b47c38c0c0fac111ba978ab7d5af9f2a`.

---

## The supported topology

```
                        Internet
                            |
                    80/443  v
                  +---------------------+
                  |  HTTPS reverse      |   nginx (default) or Caddy (profile)
                  |  proxy              |   TLS terminates here
                  +----------+----------+
                             | austro_edge
                             v
                  +---------------------+
                  |  AUSTRO OS API      |   api:8080, no published port
                  +----------+----------+
                             |
        +--------------------+--------------------+
        |                    |                    |
        v                    v                    v
  +-----------+        +-----------+        +-----------+
  |PostgreSQL |        |  Redis    |        | RabbitMQ  |   austro_internal
  | +pgvector |        |           |        |           |   (internal: true)
  +-----------+        +-----------+        +-----------+
        ^
        |
  +-----+---------------+
  |  AUSTRO OS Worker   |   separate container, no socket
  +---------------------+
```

- The **API** and the **worker** are separate long-running containers built from
  one image, so they cannot drift onto different revisions of the same code.
- Only the **reverse proxy** publishes ports. PostgreSQL, Redis and RabbitMQ
  publish none at all and sit on an internal network with no route off the host.
- The API is not published either. The proxy reaches it by service name over the
  compose network, which is the only ingress it needs.

`docker-compose.production.yml` is that topology, executable.

### The dispatch loop has exactly one owner

A third long-running role exists in every topology: the loop that sweeps due AI
retries out of PostgreSQL and publishes them to the execution queue. Which
process owns it is decided by two settings that have to agree —
`AUSTRO_PROCESS_ROLE` names what the process *is*, and
`AUSTRO_EXECUTION_DISPATCH_MODE` says what that process does with the sweep.

Hosts that need the API to sit behind a managed TLS front end — where the API
process cannot also be the supervised long-running one — can run that loop
separately, using the same `austro-api` binary under the `dispatcher` role. It
sweeps, opens **no** listener, and needs no port:

| `AUSTRO_PROCESS_ROLE` | `AUSTRO_EXECUTION_DISPATCH_MODE` | Sweeps? | Serves HTTP? | Opens a port? |
|---|---|---|---|---|
| `api` (default) | `api` (default) | yes | yes | yes |
| `api` | `off` | no | yes | yes |
| `dispatcher` | `service` | yes | no | no |
| `worker` | `off` | no | no | no |

Every other pairing is refused at startup. The role is the statement about what
the process is; the mode is one of its properties, and a combination that lets a
process do work it did not declare is a configuration error rather than a
topology to be tolerated.

`AUSTRO_EXECUTION_DISPATCH_MODE=off` on the API is a legitimate production
configuration — it is how a site runs while its dispatcher is a separately
supervised Service. It is not silent, though: the site must name the companion
in `AUSTRO_EXPECT_COMPANION_PROCESSES`, and `/health/ready` then answers `503`
when that companion stops reporting. A site that delegates the sweep and does
not say so is refused at startup, so "the API stopped dispatching" cannot be
introduced by a typo.

An unrecognised mode or role is rejected at startup rather than treated as
`api`. That matters more than it looks: silently falling back would put the
sweep loop back in the HTTP process — the exact placement the setting exists to
move away from — while reporting a healthy start. Exactly one process must run
the loop; two owners would claim the same retries.

---

## Supported targets

### 1. Single Linux host with Docker Compose — **SUPPORTED (primary)**

The model `docker-compose.production.yml` implements, and the one this
repository's scripts are written for.

| Requirement | Detail |
|---|---|
| Host OS | Any Linux the Docker Engine supports (x86-64 as built; see *Architecture* below) |
| Docker Engine | With the `docker compose` v2 plugin and a reachable daemon |
| Host ports | 80 and 443 free; they are the only ports a deployment exposes |
| Disk | Sized for PostgreSQL growth plus backups — see the volume table below |
| Outbound network | Required only if a non-stub AI or publishing backend is configured |
| Inbound network | 80/443 from the internet (or your load balancer); nothing else |

### 2. Single Linux VM in a cloud provider — **SUPPORTED**

Identical to (1): a VM is a Linux host. No provider-specific integration is
required or used, which is deliberate — there is no cloud SDK in the dependency
graph and no provider API call anywhere in the startup path.

### 3. On-premises / air-gapped host — **SUPPORTED with conditions**

The application has no mandatory outbound dependency: the default AI and
publishing backends (`stub`) are offline and deterministic, so the whole system
runs with no egress at all.

Conditions:

- The **image must be built or transferred** where a registry is reachable.
  Building requires the Go module proxy, which an air-gapped host cannot reach.
- If ACME certificates are wanted, the **Caddy** profile needs egress; on an
  air-gapped host use the default nginx proxy with operator-issued certificates
  (the normal case there anyway).

### 4. Behind an existing load balancer or ingress — **SUPPORTED with conditions**

Point the balancer at this host's 443 and let the bundled proxy terminate TLS.

- Set `AUSTRO_HEALTHCHECK_SKIP_PROXY=0` and keep the bundled proxy in the path,
  or terminate TLS upstream and run the API unpublished — the API accepts only
  HTTP, so an upstream terminator must forward plaintext to it over a network you
  already trust.
- **Do not** add a second hop that sets `X-Forwarded-*` expecting this layer to
  preserve it: the bundled proxy overwrites those headers from the peer it
  observes. If you need a real client address behind your own balancer, that is
  `CONFIGURATION REQUIRED` and is not configured here today.

### 5. External / managed PostgreSQL — **SUPPORTED with conditions**

The application talks to any PostgreSQL 16 with the `vector` extension. The
conditions are not optional, because they are the security model:

- The deployment must be able to **create the runtime and admin roles** and
  **own the schema** as the configured owner. A managed service that forbids
  `CREATE ROLE` cannot host this application's topology.
- The runtime role must be able to exist as a **non-superuser without
  `BYPASSRLS`** and must own nothing. A provider that only offers a single
  superuser connection cannot host this application.
- `pgvector` must be installable.

Set `AUSTRO_POSTGRES_DSN` and `AUSTRO_POSTGRES_RUNTIME_DSN` explicitly in that
case (the compose file derives them from parts only for its own `postgres`
service), and append the provider's required TLS parameters — for example
`?sslmode=require`. Note that the derived DSNs use `sslmode=disable`, which is
correct only on the private compose bridge.

The bundled `postgres` service enforces host authentication through the managed
`deploy/postgres/pg_hba.conf` (`hba_file` applied at every server start): `trust`
only on the in-container Unix socket, `scram-sha-256` on loopback and on every
other host — so the healthcheck's runtime-credential check really authenticates.
That contract is internal to the bundled deployment. With an external provider,
authorization is the provider's configuration: the application never relies on
`trust`, and a provider-issued credential that does not authenticate simply fails
the DSN connection at startup.

### 6. Single ARM64 Linux VM — native system services (no containers) — **SUPPORTED**

A zero-container deployment path in `deploy/native/`: the API, the worker,
PostgreSQL 16 + pgvector, Redis and RabbitMQ all run as native systemd
services on one Ubuntu 24.04 ARM64 host, with nginx as the only public
ingress. It is designed for provider Always Free AArch64 shapes — notably
Oracle Cloud's — and matches the Compose path's security model: loopback-only
datastores, scram on loopback, a non-superuser runtime role without
`BYPASSRLS`, RLS enabled and forced, and a fail-closed TLS-only proxy.

- No Docker is required on the host and no Docker is required where a release
  is built; the release is a statically linked AArch64 ELF pair produced with
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`.
- Releases are installed by `deploy/native/install-release.sh` (checksum,
  atomic symlink switch, health-gate, rollback). Full runbook:
  [free-native-deployment.md](free-native-deployment.md); operations:
  [native-operations.md](native-operations.md).
- This is a **single-host** path: it keeps the "one PostgreSQL primary"
  invariant and does not scale horizontally. If requirements later imply a
  distributed scheduler workload, the Compose path is still the deployment
  unit — see *Not supported* below.

### 7. AlwaysData shared hosting — **SUPPORTED with conditions**

A managed-PaaS path in `deploy/alwaysdata/`, using AlwaysData's managed
PostgreSQL and RabbitMQ. It exists because some AlwaysData plans cannot run
containers, and because a TLS front end is provided by the platform rather than
by a process on the host.

Three long-running processes, from two binaries:

| Unit | Platform type | Binary | Dispatch mode | Port |
|---|---|---|---|---|
| `austro-api` | User Program **site** | `austro-api` | `off` | the site's `$IP:$PORT`, behind platform TLS |
| `austro-dispatcher` | **Service** | `austro-api` | `service` | none |
| `austro-worker` | **Service** | `austro-worker` | — | none |

Conditions, all of which are requirements of the platform rather than
preferences:

- The plan must be **Small or above**. A Free plan cannot run Services at all.
  Scheduled Tasks are not a substitute: a Task whose previous run has not
  finished is skipped, so a Task cannot be a durable consumer.
- **The API must be a site, not a Service.** A Service port (`8300-8499`) is
  reachable from the internet with no network filtering, so publishing the API
  there would expose it in plaintext and bypass the site's TLS. This is the
  reason the dispatcher is a separate process, and the reason the dispatch mode
  setting above exists.
- On **Public Cloud**, Services are IPv6-only and run on hosts separate from the
  HTTP and SSH servers, so outbound reachability to the managed PostgreSQL and
  RabbitMQ has to hold from the site *and* from both Services. On **Private
  Cloud** the `8300-8499` range is not externally reachable at all.
- **Redis is unresolved on Public Cloud.** AlwaysData offers managed Redis on
  Private Cloud only, and the self-hosted alternative is a Service port, i.e.
  internet-reachable. This is called out in
  [alwaysdata/RUNBOOK.md](../deploy/alwaysdata/RUNBOOK.md) and must be decided
  explicitly before deploying.
- Platform resource limits are only visible in the panel on Private Cloud, so
  host sizing is not verifiable in advance on Public Cloud.

Restart semantics are part of the contract, not an implementation detail: the
platform restarts a Service by sending `SIGHUP`, and a Service that fails
repeatedly in a short period is **automatically disabled** and stops being
restarted. Both binaries handle `SIGHUP` as a graceful shutdown, so in-flight
requests and unacknowledged broker messages are settled rather than dropped,
and monitoring commands are left unset — a flaky monitor on this platform
converts a blip into a restart loop and then into a disabled service.

Full runbook, including the platform capability table and the account checks
that must be completed first:
[alwaysdata/RUNBOOK.md](../deploy/alwaysdata/RUNBOOK.md). **The topology is
documented; no part of it has been exercised on a real AlwaysData account.**

---

## Not supported

| Target | Status | Why |
|---|---|---|
| Container-orchestration platforms | **NOT SUPPORTED** | No manifests, no Helm chart, no operator. `docker-compose.production.yml` is the deployment unit. Nothing here is designed to run as a distributed scheduler workload. |
| Serverless / PaaS function hosts | **NOT SUPPORTED** | The API and worker are long-running processes with in-process connection pools, a RabbitMQ consumer and a graceful drain. Vercel in particular is **NOT SUPPORTED as a backend**; adding `vercel.json` is explicitly out of scope. |
| Kafka-based event transport | **NOT SUPPORTED** | The transport is RabbitMQ (`github.com/rabbitmq/amqp091-go`). No Kafka client exists in the dependency graph and no compatibility shim is provided. |
| Microservice split | **NOT SUPPORTED** | This is a Go modular monolith. Splitting the API and worker into independently versioned services is not a supported deployment. Running one binary in two process roles is not this: see *The dispatch loop has exactly one owner* above. |
| Shared hosting without managed long-running processes | **NOT SUPPORTED** | A host that offers no Service equivalent and only scheduled tasks cannot run the RabbitMQ consumer or the dispatch loop: a Task whose previous run is unfinished is skipped. A platform that terminates TLS and supervises foreground processes is required. |
| Docker Swarm | **NOT SUPPORTED** | Not tested, and the compose file uses no Swarm-only keys. |
| Multi-region active-active | **NOT SUPPORTED** | There is one PostgreSQL primary. The audit chain is a linear hash chain, so more than one writer against separate databases would produce two chains, not one. |
| Read replicas / connection pooler in front of PostgreSQL | **NOT SUPPORTED as shipped** | No PgBouncer or equivalent is configured, and transaction-scoped session state (`SET LOCAL`, RLS workspace binding) makes a statement-level pooler a correctness risk rather than a tuning knob. |
| Windows containers | **NOT SUPPORTED** | The image is Linux-only. |

---

## Host sizing

Starting points, not measured benchmarks. The workload is small JSON documents,
one PostgreSQL primary and a single worker consumer; the memory figures are
dominated by PostgreSQL's shared buffers and the container toolchain, not by the
Go processes.

| Resource | Minimum | Comfortable | Notes |
|---|---|---|---|
| vCPU | 2 | 4 | PostgreSQL and the vector index builds are the CPU-bound parts |
| RAM | 4 GB | 8 GB | PostgreSQL `shm_size` is set to 256 MB in the compose file |
| Disk | 40 GB | 100 GB+ | PostgreSQL data, its WAL, and backups |
| Disk type | SSD | SSD/NVMe | The vector similarity search is I/O sensitive |

**Architecture:** the Dockerfile builds `GOOS=linux GOARCH=amd64`. On arm64
(for example an Apple Silicon development host or an ARM VM) either build with
`--build-arg`-free native settings after changing `GOARCH`, or use the amd64
image under emulation (slow, unverified). The container image is documented as
`NOT TESTED` on arm64; the **native path** (target 6) is the tested ARM route —
its release binaries are built and byte-verified for `linux/arm64` in CI.

---

## Persistent state

| Volume | Contents | Authoritative? |
|---|---|---|
| `postgres_data` | Workspaces, tasks, knowledge, memory embeddings, publications, pipelines, users, departments, teams, AI employees, and the audit chain | **Yes.** This is the system of record. |
| `redis_data` | Workspace memory bank entries | No. Rebuildable from PostgreSQL; the volume avoids losing it on an ordinary restart, it is not a recovery guarantee. |
| `rabbitmq_data` | Queue definitions and undelivered messages | No. In-flight transport state, not durable application state. |
| `caddy_data` | ACME account key and issued certificates (Caddy profile only) | Not application data; losing it forces re-issuance. |

Backup scope follows directly from this table: **PostgreSQL only**. See
[backups-and-restore.md](backups-and-restore.md).

---

## What this document does not claim

Deployments on these targets have **not** been exercised end to end on any host.
The authoring environment had no Docker daemon (recorded as `BLOCKED` in
[FINAL_PRODUCTION_READINESS_REPORT.md](FINAL_PRODUCTION_READINESS_REPORT.md)),
but CI does run the real stack:

- the `production-smoke` job brings up postgres, redis, rabbitmq, api and
  worker, runs the operator's own `scripts/healthcheck.sh` against them, and
  asserts the datastores and the API are unreachable from the host;
- the `production-rehearsal` job goes further: it deploys with
  `scripts/deploy.sh` **including the HTTPS reverse proxy** (served with
  ephemeral self-signed TLS), runs the full `scripts/healthcheck.sh`, and then
  rehearses a real backup and restore cycle (`scripts/backup.sh` ->
  `scripts/restore.sh`) with schema-valid application data, verifying the data
  and the RLS/runtime-role topology after the restore.

So target (1) is **implemented and verified to bring up a real, correctly
isolated topology in CI, over TLS, with a working backup/restore cycle** — but
CI is not your host. It still does not exercise a real public hostname, real
public-DNS resolution, operator-issued certificates, real internet traffic, or
your host's volumes and backup storage. Treat it as **not proven on your host**
until you have run `scripts/deploy.sh` and `scripts/healthcheck.sh` there
yourself.

The native path (target 6) has the same honest boundary, in the other
direction: its release is **build-verified** (AArch64 ELF, static) in CI, but
no line of it has run on a real VM. Per-capability statuses are recorded in
[FREE_DEPLOYMENT_CERTIFICATION.md](FREE_DEPLOYMENT_CERTIFICATION.md); every VM
stage is `OPERATOR CONFIGURATION REQUIRED` until an operator executes it.

The AlwaysData path (target 7) has the strongest boundary of the three, and it
is a different kind of gap. Its platform facts are cited from AlwaysData's own
documentation, and its process topology is the one the code actually supports
— but **no part of it has been run on a real account**. There is no target
account whose plan, cloud type, Service entitlement, outbound routing from
either the site or the Services, or resource ceiling has been confirmed, and the
Redis question above is genuinely open. Treat target 7 as an architecture that is
ready to be tried and not as a deployment that works.
