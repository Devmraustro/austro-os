# AUSTRO OS — Backups and Restore

What is backed up, how often, where it goes, how to restore it, and — stated
plainly — what this repository has and has not actually proven about recovery.

---

## What is authoritative, and what is not

| Store | Contents | Backed up? | Why |
|---|---|---|---|
| **PostgreSQL** | Workspaces, users, tasks, knowledge documents, memory embeddings, publications, pipelines, departments, teams, AI employees, **and the audit chain** | **Yes — this is the backup** | It is the system of record. If a row only exists here, losing it is losing data. |
| **Redis** | Workspace memory bank entries | **No** | **Redis is not the authoritative application datastore.** It is a working store that the application treats as rebuildable from PostgreSQL. It is given a volume so an ordinary restart does not silently discard workspace memory — that is a convenience, not a recovery guarantee. |
| **RabbitMQ** | Queue definitions and undelivered messages | **No** | **RabbitMQ's transient state is not the same as PostgreSQL persistence.** An undelivered in-flight event is transport state. The durable record of what happened is the audit chain in PostgreSQL. |
| **Caddy / TLS material** | ACME account key and issued certificates | **No** | Not application data. Losing it forces re-issuance, which is inconvenient and rate-limited, but is not data loss. |

A backup that included Redis and RabbitMQ would imply a recovery guarantee the
system does not make. Scope follows the authority table, not convenience.

---

## Backup contents

`scripts/backup.sh` runs `pg_dump` as the owner role over the PostgreSQL
container's local socket, with `--clean --if-exists`, and compresses it:

```
backups/austro-postgres-20260917T120000Z.sql.gz
backups/austro-postgres-20260917T120000Z.sql.gz.sha256
```

| Property | Value |
|---|---|
| Format | Plain SQL dump, gzip -9 compressed |
| Self-contained | Yes — `--clean --if-exists` emits the drops, so restoring into a non-empty database replaces rather than fails |
| Includes | Schema, data, indexes, constraints, **RLS policies and enabled/forced flags** as `pg_dump` emits them, and grants |
| Excludes | Cluster roles (`austro_app`, `austro_app_admin`) — `pg_dump` does not dump roles. The application's bootstrap recreates/repairs them at startup, which is why a restore is followed by an API restart. |
| Credentials in the file | None, beyond the password hashes already stored in the `users` table |
| Credential needed to take it | None — the local socket is configured for local trust, so no database password enters the script's environment, argument list or output |

**Verification at write time.** The script refuses to report success unless the
artifact is (1) non-empty, (2) an intact gzip stream (`gzip -t`), and (3) contains
a real `PostgreSQL database dump` header. It then writes a sha256 of the file
next to it. `pipefail` is what stops a `pg_dump` that died halfway from being
reported as a successful backup.

---

## Frequency and retention

| Setting | Default | Where |
|---|---|---|
| Schedule | **Your scheduler, not this repository's** — a cron entry or a CI schedule | `CONFIGURATION REQUIRED` |
| Retention | 14 days, pruned by the script | `AUSTRO_BACKUP_RETENTION_DAYS` |
| Local directory | `./backups` | `AUSTRO_BACKUP_DIR` |

A suggested baseline: **nightly full backup**, retain 14 daily; keep one copy
off-host; take an additional **pre-migration backup** before every deployment
that changes the schema (and note that `scripts/deploy.sh` already takes one
automatically whenever a database exists).

The script is safe to run while the system is up. `pg_dump` takes a consistent
snapshot; it does not stop the application.

Suggested cron entry:

```cron
# Nightly at 02:15, log to syslog-visible file
15 2 * * * cd /path/to/austro-os && ./scripts/backup.sh >> /var/log/austro-backup.log 2>&1
```

---

## Off-host copies

Local backups share a failure domain with the database they protect: a host loss
loses both. Off-host copying is enabled **only** when a bucket is explicitly
configured.

```bash
AUSTRO_BACKUP_S3_BUCKET=my-austro-backups
AUSTRO_BACKUP_S3_PREFIX=austro-os/
AUSTRO_BACKUP_S3_REGION=eu-west-1     # optional; the AWS CLI also reads AWS_REGION
```

When `AUSTRO_BACKUP_S3_BUCKET` is unset, the script prints:

```
no AUSTRO_BACKUP_S3_BUCKET configured — backup is LOCAL ONLY (same host as the database)
```

That line is deliberate: a backup routine that is silent about being local-only
invites the reader to assume redundancy that does not exist. When a bucket *is*
configured but the upload fails, the script exits non-zero (the local file is
left intact and reported) — a failed off-host copy is not reported as success.

Requires the `aws` CLI on the host. **No other destination is supported**:
no GCS, Azure Blob or rsync target ships today.

---

## Encryption

**Not implemented by these scripts — `NOT SUPPORTED` as a built-in.**

- At rest: rely on the storage layer (LUKS, an encrypted volume, or the object
  store's server-side encryption). The `.sql.gz` file is **not** encrypted by the
  scripts. **CONFIGURATION REQUIRED** on your side.
- In transit: the S3 upload uses TLS via the AWS CLI defaults. No custom
  verification is configured here.
- Encryption expectations to state in your own runbook: the dump contains every
  workspace's data and the password hashes in `users`. Anything that can read the
  file can read all of it. If your policy requires encryption at rest irrespective
  of the storage layer, encrypt before it leaves the host — for example
  `age -r <recipient> backup.sql.gz > backup.sql.gz.age` as a wrapper — and
  decrypt before restoring, because `scripts/restore.sh` expects a plain `.sql.gz`.

Do not put a `GPG`/`age` key on the same host as the backups without thinking
about it: a key stored beside the ciphertext is decoration, not protection.

---

## Restore procedure

`scripts/restore.sh` **replaces** the contents of the production database. It
refuses to run without explicit confirmation.

```bash
# 1. Choose the artifact.
ls -1t backups/ | head

# 2. Confirm its checksum if one is present (the script checks this too).
sha256sum -c backups/austro-postgres-<ts>.sql.gz.sha256

# 3. Restore. Interactive, or set AUSTRO_RESTORE_CONFIRM=yes for unattended use.
scripts/restore.sh backups/austro-postgres-<ts>.sql.gz
```

What it does, in order:

1. **Validates the artifact** — non-empty, valid gzip, real dump header, and the
   sha256 matches if a `.sha256` file sits beside it. A restore never starts from
   an unverified file.
2. **Confirms destructively** — warns that every row written since the backup
   will be lost; requires `AUSTRO_RESTORE_CONFIRM=yes` or the database name typed
   at an interactive prompt.
3. **Quiesces the application** — stops the worker *first*, then the API, and
   refuses to continue if either is still running. Stopping them the other way
   would leave the worker committing while the API is down.
4. **Restores** with `psql -v ON_ERROR_STOP=1`, so the first failing statement
   aborts instead of leaving a half-restored database with an exit status of 0.
5. **Restarts the API** so the idempotent bootstrap re-applies schema migrations,
   role provisioning, RLS policies and forced RLS to the restored tables. This
   step is what turns a copy of some tables into a working AUSTRO OS database.
6. **Verifies** (below).
7. **Restarts the worker**, then runs `scripts/healthcheck.sh`.

### Restore verification

| Check | Asserted |
|---|---|
| Tables | all 12 protected tables present |
| pgvector | the `vector` extension is installed |
| RLS enabled | ≥ 12 tables, evaluated **as the runtime role** |
| RLS forced | ≥ 12 tables |
| Runtime role | not a superuser, does not hold `BYPASSRLS` |
| Ownership | the runtime role owns **0** protected tables |
| Runtime credential | the runtime role can actually authenticate |
| Audit schema | `seq`, `workspace_id`, `timestamp_canonical` all present |
| Audit readability | `audit_events` is readable; the row count is reported |

Two honest notes about that table:

- The audit **row count is reported, not asserted against a floor**. A
  legitimately empty organisation has no audit rows, and failing a restore for
  that would be a false alarm. What is asserted is that the persistent-audit
  schema survived.
- **Full hash-chain verification is not performed by the script.** That is the
  application's job, at `GET /audit/verification`, and it requires an
  authenticated founder session. Run it manually after a restore if chain
  integrity is what you are there to confirm.

---

## Restore verification exercise

A backup you have never restored is an assumption. **The CI `production-rehearsal`
job now performs a real restore exercise on an isolated runner on every run**:
it deploys the stack, seeds schema-valid application data, takes a real backup
with `scripts/backup.sh`, destroys the data, then restores it with
`scripts/restore.sh` and verifies the data plus the RLS/runtime-role topology
came back. The previously-false claim "CI has never performed a restore" has
therefore been superseded; a restore that fails in CI now fails the gate.

That is still not the same as disaster recovery on your host. A CI runner is an
ephemeral, disposable host with its own volumes and no public DNS, so the
rehearsal proves the *mechanism*, not your *environment*. The quarterly exercise
below remains the only thing that produces an actual recovery time objective.
Run it quarterly, on a scratch host that shares no volumes with production:

1. Provision a scratch host with the same compose file and a **fresh**
   `.env.production` (different hostname, no public ingress).
2. Copy the newest backup and its `.sha256` to it.
3. `scripts/deploy.sh` to bring up an empty stack, then `scripts/restore.sh <backup>`.
4. Confirm every check in the table above passes.
5. Log in as the founder and confirm real records are visible — a workspace, a
   task, an AI employee.
6. Query `GET /audit/verification` and confirm the chain verifies.
7. Record the elapsed time. **That number is your actual recovery time
   objective**; anything else is a hope.
8. Tear the scratch host down and delete the copy.

---

## Pre-migration backups

Schema migrations are applied automatically by the application at startup and are
idempotent, but "idempotent" is not "reversible". Before any release that changes
the schema:

```bash
scripts/backup.sh
```

`scripts/deploy.sh` also does this automatically whenever a database already
exists, and **refuses to deploy** if the backup fails — a deployment that
destroys data without a recoverable copy is a worse outcome than a failed
deployment.

---

## Limitations to state out loud

- Recovery point: everything committed since the last successful backup is lost.
  Nightly backups mean up to 24 hours. There is **no point-in-time recovery**:
  WAL archiving is not configured. `NOT SUPPORTED`.
- The dump is a logical snapshot in plain SQL; restoring a large database is
  slower than a physical restore and gets slower with size.
- Backups are unencrypted by the scripts (see *Encryption*).
- Off-host copies go to S3 only, and only if you configure a bucket.
- Nothing here verifies that a backup is restorable *until you restore it* —
  the write-time checks prove the artifact is a well-formed dump, not that a
  restore will succeed against your schema and extensions.
