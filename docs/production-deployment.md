# AUSTRO OS — Production Deployment

How to take this repository from a clean checkout to a running, verified
production deployment, and how to update or roll it back afterwards.

Read [deployment-targets.md](deployment-targets.md) first if you have not
decided where this will run. This document assumes target (1): a single Linux
host with Docker Compose.

---

## 0. What you need before you start

| # | Item | Notes |
|---|---|---|
| 1 | A Linux host with Docker Engine and the `docker compose` v2 plugin | The deploying user must be able to reach the Docker daemon. |
| 2 | Ports 80 and 443 free on that host | Nothing else is exposed. |
| 3 | A DNS record for your hostname pointing at the host | Required for TLS to validate. |
| 4 | A TLS certificate and key, **or** the Caddy profile | `CONFIGURATION REQUIRED`. See step 3. |
| 5 | The generated secrets | `openssl rand -hex 32` — one per secret. |
| 6 | This repository checked out on the host | The scripts and compose file are the deployment unit. |

Check your host is ready:

```bash
docker version
docker compose version
```

---

## 1. Get the code

```bash
git clone https://github.com/Devmraustro/austro-os.git
cd austro-os
git checkout main          # main is the authoritative baseline
git rev-parse HEAD         # expect 64c72ba102c570241c9ec594d466a8e4bbc0ff6d at the time of writing
```

Deploy a **tag or a specific commit**, not a moving branch, so a rollback target
is unambiguous and reproducible.

---

## 2. Create the production configuration

```bash
cp .env.example .env.production
```

Open `.env.production` and fill in every **REQUIRED** value. At minimum:

```bash
# Generate secrets — do NOT reuse the placeholders.
for name in AUSTRO_POSTGRES_PASSWORD AUSTRO_POSTGRES_RUNTIME_PASSWORD \
            AUSTRO_REDIS_PASSWORD AUSTRO_RABBITMQ_PASSWORD \
            AUSTRO_JWT_SECRET AUSTRO_JWT_REFRESH_SECRET; do
  echo "$name=$(openssl rand -hex 32)"
done
echo "AUSTRO_FOUNDER_PASSWORD=$(openssl rand -base64 24)"
```

Rules the deployment will enforce, so it is faster to satisfy them now:

- `AUSTRO_POSTGRES_PASSWORD` ≠ `AUSTRO_POSTGRES_RUNTIME_PASSWORD`
- `AUSTRO_JWT_SECRET` ≠ `AUSTRO_JWT_REFRESH_SECRET`, each ≥ 32 bytes
- `AUSTRO_FOUNDER_PASSWORD` ≥ 16 characters, and neither founder value alone
- No value containing `change-me`, `example`, or a loopback host

Set `AUSTRO_PUBLIC_HOSTNAME` to the real hostname. Full contract:
[production-configuration.md](production-configuration.md).

> `.env.production` **must exist**: the compose file references it through
> `env_file:` so a missing file fails immediately instead of starting a stack
> with half its configuration. It is git-ignored and must never be committed.

---

## 3. Provide TLS material

**nginx (default).** Create the directory and place exactly these two files in it:

```bash
mkdir -p deploy/tls
# fullchain.pem  — the server certificate followed by any intermediates
# privkey.pem    — the private key, unencrypted
chmod 600 deploy/tls/privkey.pem
```

`AUSTRO_TLS_CERT_DIR` (default `./deploy/tls`) names the host directory; it is
mounted read-only at `/etc/nginx/tls`. nginx exits immediately if either file is
missing, so a bad mount fails the container rather than serving plaintext.

If you have no certificate yet, obtain one from your CA, or use the Caddy
variant in the next step.

**Caddy (optional, automatic TLS).** Set in `.env.production`:

```bash
AUSTRO_PROXY_SERVICE=reverse-proxy-caddy
AUSTRO_PUBLIC_HOSTNAME=app.yourdomain.com
AUSTRO_ACME_EMAIL=ops@yourdomain.com
```

Caddy requests and renews certificates itself; ports 80/443 must be reachable
from the internet for the ACME challenge, and the hostname must resolve here.
Certificates persist in the `caddy_data` volume — **do not** delete it casually,
as re-issuance is rate-limited.

---

## 4. Deploy

```bash
scripts/deploy.sh
```

It will, in order:

1. check Docker and the configuration, and refuse to continue on any placeholder,
   short secret, or identical secret pair — reporting the setting **name**, never
   its value;
2. run `docker compose config -q` to resolve the whole file before anything is
   created;
3. require the TLS material when the nginx proxy is selected;
4. back up PostgreSQL first **if a database already exists** (a first deployment
   says so rather than reporting a skipped backup as if it had happened);
5. start `postgres`, `redis`, `rabbitmq` and wait for each to report healthy;
6. start `api` and wait for health — this is where schema migrations, role
   provisioning and the runtime-privilege verification happen;
7. start `worker` and wait for health;
8. start the reverse proxy;
9. run `scripts/healthcheck.sh`, which checks the live topology rather than the
   configuration.

Any failure returns non-zero and prints recent logs for the service that failed.

Expected duration: a couple of minutes on a warm host; longer on a cold database,
because the first API start applies the schema and provisions the RLS role
topology before it opens its listener.

---

## 5. Verify for yourself

A non-zero deploy already means something is wrong, but confirm the claims
directly:

```bash
scripts/healthcheck.sh              # full topology check
docker compose --env-file .env.production -f docker-compose.production.yml ps
curl -I https://app.yourdomain.com/health/live
```

Confirm that nothing beyond the proxy is reachable from outside:

```bash
# These must ALL fail or time out from another machine.
nc -zv app.yourdomain.com 5432    # PostgreSQL
nc -zv app.yourdomain.com 6379    # Redis
nc -zv app.yourdomain.com 5672    # RabbitMQ AMQP
nc -zv app.yourdomain.com 15672   # RabbitMQ management UI
```

If any of them connects, stop and fix the firewall or the compose file before
going further.

---

## 6. First-run bootstrap

The founder identity is created once, through the running API:

```bash
curl -sS -X POST -H 'Content-Type: application/json' -d '{}' \
  https://app.yourdomain.com/api/auth/bootstrap
```

It returns **201** and uses the configured `AUSTRO_FOUNDER_USERNAME` /
`AUSTRO_FOUNDER_PASSWORD`. It refuses with 409 once a founder exists, so it is
not a repeated credential-reset path.

Then log in and change nothing else:

```bash
curl -sS -X POST -H 'Content-Type: application/json' \
  -d '{"username":"founder","password":"<your founder password>"}' \
  https://app.yourdomain.com/api/auth/login
```

Keep the founder credential in your password manager. It is the only
organisation-level identity, and there is no self-service recovery for it.

---

## Updating a running deployment

```bash
cd austro-os
git fetch --tags
git checkout <new-tag-or-commit>     # pin the target
scripts/deploy.sh
```

`scripts/deploy.sh` takes a pre-deployment backup automatically, so the previous
state is recoverable. Schema migrations are applied automatically at API start
and are idempotent — there is no separate migration step and no migration
container.

If the update is a schema-affecting one, take your own labelled backup first so
the artifact is identifiable in `./backups/`:

```bash
scripts/backup.sh
```

---

## Rolling back

Rollback is **code + restore**, not code alone, whenever the release changed the
schema:

```bash
# 1. Return to the previous revision.
git checkout <previous-tag-or-commit>

# 2. Restore the pre-deployment backup taken automatically by deploy.sh.
ls -1t backups/ | head
scripts/restore.sh backups/austro-postgres-<timestamp>.sql.gz

# 3. Redeploy at that revision.
scripts/deploy.sh
```

Why the restore matters: the previous schema and its RLS policies are not
restored by checking out old code. `scripts/restore.sh` replaces the schema and
then restarts the API so the idempotent bootstrap re-establishes the policies,
grants and forced RLS for the restored tables — and it verifies that before
declaring success.

---

## Shutting down and starting back up

```bash
# Stop everything, keep the data.
docker compose --env-file .env.production -f docker-compose.production.yml down

# Start again (the same order deploy.sh uses).
scripts/deploy.sh
```

`down -v` **deletes the volumes**, including `postgres_data`. That is a full data
loss, not a cleanup step.

---

## Deployment checklist

Copy this into your change record.

```
[ ] Target commit/tag recorded: ______________________
[ ] .env.production present, every REQUIRED value set, none are placeholders
[ ] Backups directory exists on a disk with room, or a bucket is configured
[ ] TLS material in place (or the Caddy profile selected and DNS verified)
[ ] scripts/deploy.sh exited 0
[ ] scripts/healthcheck.sh exited 0 with 0 failures
[ ] Ingress check: 5432/6379/5672/15672 unreachable from outside
[ ] https://<hostname>/health/live returns 200
[ ] Founder login verified (or bootstrap performed if this is the first deploy)
[ ] Rollback target identified: ______________________
```

---

## What a successful deployment does NOT establish

Stated plainly, because it is easy to over-read a green run:

- It does **not** mean the application has been validated under load. No load
  testing has been performed.
- It does **not** mean disaster recovery has been tested end to end. A restore
  exercise is described in [backups-and-restore.md](backups-and-restore.md) and
  must be performed by you, in your environment, to count.
- It does **not** mean the system is "fully secure". It means the specific
  controls this repository implements (no public datastore ports, non-root
  containers, RLS enforced for a non-owner runtime role, TLS 1.2+ with HSTS,
  secrets not committed) are in place and machine-checked. Controls that are not
  implemented are listed in
  [FINAL_PRODUCTION_READINESS_REPORT.md](FINAL_PRODUCTION_READINESS_REPORT.md)
  rather than omitted.
