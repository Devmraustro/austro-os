# AUSTRO OS — Free Native Deployment (Oracle Cloud Always Free)

The zero-container path to production for a single cloud VM. The full
application topology — AUSTRO OS API, AUSTRO OS Worker, PostgreSQL 16 +
pgvector, Redis, RabbitMQ — runs as **native systemd services** on one Ubuntu
24.04 LTS ARM64 host, with nginx terminating TLS as the only public ingress.
No Docker is required on the VM and no Docker is required on the machine that
produces a release.

This is deliberately aligned with the Oracle Cloud Infrastructure (OCI) Always
Free tier: an AArch64 VM shape is itself free of charge, and this runbook
limits the running footprint to what the Always Free quota grants.

> Exact per-capability status is in
> [FREE_DEPLOYMENT_CERTIFICATION.md](FREE_DEPLOYMENT_CERTIFICATION.md).

---

## 1. Topology

```
                         Internet
                             |
                       80/443 v
                  +---------------------+
                  |   nginx (default)   |   /etc/nginx/nginx.conf
                  |   TLS here          |   certs /etc/austro/tls
                  +----------+----------+
                             | 127.0.0.1:8080 HTTP
                             v
                  +---------------------+
                  |   AUSTRO OS API     |   austro-api.service  (User=austro)
                  +----------+----------+
                             |
                  +----------+----------+
                  |   AUSTRO OS Worker  |   austro-worker.service
                  +----------+----------+
                             |
        +--------------------+--------------------+
        |                    |                    |
        v                    v                    v
  +-----------+        +-----------+        +-----------+
  |PostgreSQL |        |  Redis    |        | RabbitMQ  |   all loopback only
  | 16+pgext  |        |  auth     |        |  auth     |
  +-----------+        +-----------+        +-----------+
```

Security properties, mirroring the Docker Compose path:

- PostgreSQL, Redis and RabbitMQ bind **loopback only**. Nothing on the host or
  the network can reach them except through authentication on `127.0.0.1`.
- nginx is the only process listening on 80/443. It is **fail-closed**: it is
  not enabled until real TLS material exists in `/etc/austro/tls`.
- The API listens on `127.0.0.1:8080` and is not reachable off-host except via
  nginx.
- The app's role model is unchanged: schema owner `austro`, runtime role
  `austro_app` (non-superuser, **no BYPASSRLS**, owns nothing), RLS enabled and
  forced on every table.
- Long-lived secrets live only in `/etc/austro/austro.env` (`0600`, root).

The provisioning and all operator scripts live in `deploy/native/` and run on
the VM itself; the release comes from CI. See the
[deploy/native README](../../deploy/native/README.md) for the file map.

---

## 2. Prerequisites and status

| Item | Required for | Status |
|---|---|---|
| An Oracle Cloud account with an Always Free ARM64 VM (A1.Flex, OCI) | Everything after this table | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` |
| Ubuntu 24.04 LTS **ARM64** image on that VM | `bootstrap-ubuntu24.sh` | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` |
| A public hostname with DNS A record → the VM's public IP | nginx TLS with an ACME or operator cert | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` |
| SSH access (key, port 22 allowed in the OCI security list) | every step below | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` |
| The GitHub environment `native-prod` + 4 repo secrets | the CI deploy job | `OPERATOR CONFIGURATION REQUIRED — NOT RUN` |
| Outbound egress on the VM to Ubuntu/OCI mirrors | `bootstrap-ubuntu24.sh` package install | depends on provider defaults |

Nothing in the authoring environment had a VM; every row above is awaiting the
operator. The repository side (release build, scripts, workflow) is
`REPOSITORY READY`.

---

## 3. Provision the VM and bootstrap the host

1. In OCI, create an **Always Free ARM64 (A1.Flex)** VM with the Ubuntu 24.04
   LTS arm64 image. Record its public IP.
2. Let port **22** through in the VCN security list (and 80/443 when you are
   ready to serve traffic; OCI Always Free also includes 10 TB/month egress).
3. SSH in and run the bootstrap. Copy the script from this repository:

   ```sh
   scp deploy/native/bootstrap-ubuntu24.sh ubuntu@<vm-ip>:/tmp/
   ssh ubuntu@<vm-ip> -- sudo -n bash /tmp/bootstrap-ubuntu24.sh
   ```

   The bootstrap is idempotent and safe to re-run. What it does:

   - installs `postgresql-16`, `postgresql-16-pgvector`, `redis-server`,
     `rabbitmq-server`, `nginx`, `curl`, `openssl`, `ufw`,
     `unattended-upgrades`;
   - creates the `austro` OS user (or reuses the existing one) and a `deploy`
     group the operator account is a member of;
   - provisions PostgreSQL: `vector` and `pgcrypto` extensions pre-created,
     role `austro` (`LOGIN CREATEROLE NOSUPERUSER NOBYPASSRLS`), listen
     `127.0.0.1`, `pg_hba.conf` with `peer` on the local socket and
     `scram-sha-256` on loopback only;
   - reconfigures Redis under a managed config: loopback only, `protected-mode
     yes`, `requirepass`, AOF with `everysec`, supervised by systemd;
   - reconfigures RabbitMQ: listener `127.0.0.1:5672`, management plugin
     disabled if present, dedicated `austro` user with full permissions on `/`,
     `guest` deleted;
   - installs `deploy/native/austro-api.service` and
     `deploy/native/austro-worker.service` under systemd;
   - enables the firewall with 22/80/443 only, and `unattended-upgrades`.

   PostgreSQL and RabbitMQ secrets are read from `/etc/austro/austro.env`; the
   bootstrap **refuses to run without it** and never guesses or prints a
   secret.

---

## 4. Create `/etc/austro/austro.env`

```sh
ssh ubuntu@<vm-ip> -- sudo -n bash /tmp/generate-env.sh
```

`generate-env.sh` (copy it from `deploy/native/`) creates the file from
`openssl rand` when it does not exist, prompts interactively for the founder
username and password (they are never echoed), applies `umask 077` +
`chmod 0600`, and prints nothing. To supply the founder credentials
non-interactively, export `AUSTRO_FOUNDER_USERNAME` and
`AUSTRO_FOUNDER_PASSWORD` before running it as root. It never overwrites an
existing file; a fresh VM always starts with an empty `/etc/austro/` so the
first run provisions it.

Every value is documented in `deploy/native/austro.env.example`, which is the
contract: all `change-me` placeholders must be replaced; this deployment's
addresses use `127.0.0.1:<port>` exactly as the example does.

Back this file up (e.g. into the operator's password manager) — it is the root
of trust and it cannot be regenerated.

---

## 5. TLS material and the fail-closed proxy

nginx stays disabled until certificates exist; there is no plaintext fallback.
Place the files and enable:

```sh
ssh ubuntu@<vm-ip> -- sudo install -d -o root -g root -m 0755 /etc/austro/tls
scp fullchain.pem privkey.pem ubuntu@<vm-ip>:/tmp/
ssh ubuntu@<vm-ip> -- sudo install -o root -g root -m 0644 \
  /tmp/fullchain.pem /tmp/privkey.pem /etc/austro/tls/
ssh ubuntu@<vm-ip> -- sudo nginx -t    # must pass; then:
ssh ubuntu@<vm-ip> -- sudo systemctl enable --now nginx
```

What the managed `nginx.conf` enforces: HTTP → HTTPS 301, TLS 1.2/1.3, HSTS,
`X-Content-Type-Options: nosniff`, `server_tokens off`, request bodies capped
above the Go 1 MiB server limit, discarded client-supplied `X-Forwarded-*`
(replaced with the observed peer), proxy timeouts — one upstream:
`127.0.0.1:8080`.

---

## 6. Produce and install a release

Two equivalent sources:

- **From CI (henceforth the supported path):** `workflow_dispatch` on the
  **AUSTRO OS Native ARM64 Deployment** workflow after setting the repo
  variable `DEPLOY_ENABLED` to `true` (see §8). CI builds, verifies byte
  format, packages, uploads the artifact, and the `deploy` job copies it and
  runs `deploy/native/install-release.sh` on the VM.
- **Locally:** use any Go 1.25.13 toolchain and build the same two binaries,
  then upload manually:

  ```sh
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o main .
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o worker ./cmd/worker
  tar -czf austro-<sha>.tar.gz main worker
  sha256sum austro-<sha>.tar.gz
  scp austro-<sha>.tar.gz deploy/native/install-release.sh ubuntu@<vm-ip>:/tmp/
  ssh ubuntu@<vm-ip> -- sudo -n bash /tmp/install-release.sh /tmp/austro-<sha>.tar.gz <sha>
  ```

`install-release.sh` verifies the 64-hex checksum against what it was asked to
install, unpacks into `/opt/austro/releases/<sha>/` (owner `austro`), switches
the `current` symlink **atomically**, restarts the API, waits for
`/health/live` (bounded), restarts the worker, and runs
`deploy/native/healthcheck.sh`. On any failure it **rolls back** to the
previous release and exits non-zero.

---

## 7. First verification

```sh
ssh ubuntu@<vm-ip> -- sudo -n bash /tmp/healthcheck.sh
# PASS: everything green (or PASS WITH BLOCKED CHECKS if only TLS was skipped)
systemctl --no-pager -p ActiveState status austro-api austro-worker postgresql redis-server rabbitmq-server nginx
```

Then optionally point `monitor.sh` at an operator webhook (see
[native-operations.md](native-operations.md)).

---

## 8. Wiring the CI deploy job (operator decision)

The `deploy` job in `.github/workflows/native-arm64-deploy.yml` only ever runs
on `workflow_dispatch` **and** `vars.DEPLOY_ENABLED == 'true'`. To enable it:

1. Create the GitHub **environment `native-prod`**.
2. Set environment secrets: `DEPLOY_HOST` (public IP or hostname),
   `DEPLOY_PORT` (22), `DEPLOY_USER` (the operator account capable of
   `sudo -n`), `DEPLOY_SSH_KEY` (a dedicated deploy key, not a personal one).
3. Set the repository variable `DEPLOY_ENABLED` to `true`.
4. Run the workflow manually, watch the `deploy` job, and confirm the
   health-gate exits clean.

Long-lived application secrets are **not** part of this flow: they live in
`/etc/austro/austro.env` on the VM and are set during provisioning.

---

## 9. What is deliberately out of scope

- This path is a **single host**. It does not scale horizontally, and it keeps
  the "one PostgreSQL primary" invariant from the Compose path (the audit chain
  is a linear hash chain).
- No container-orchestration platform is involved; if your requirements later
  imply one, the Compose path is the supported deployment unit, not this one.
- Backups are PostgreSQL-only, exactly as in the Compose path — see
  [native-operations.md](native-operations.md).

See [deployment-targets.md](deployment-targets.md) for how this target sits
among the others, and [native-troubleshooting.md](native-troubleshooting.md)
for the first-response runbook.