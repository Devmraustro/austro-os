# Founder Operations Checklist — AUSTRO OS

Operational runbook for taking the release (`PRODUCTION_READINESS_REPORT.md`,
HEAD `adb7c5d`) into service. Each item is either required before first live
traffic or the standing operational practice afterwards.

## 1. Pre-launch configuration

- [ ] Provision PostgreSQL (pgvector) and set `AUSTRO_POSTGRES_DSN` to the real
      host/port. Verify RLS is enabled on every scoped table
      (`workspaces`, `departments`, `teams`, `ai_employees`, `memory_embeddings`,
      tasks, knowledge, publications, pipelines) before migration.
- [ ] Provision Redis and set `AUSTRO_REDIS_ADDR`.
- [ ] Provision RabbitMQ and set `AUSTRO_RABBITMQ_URL` (+ `AUSTRO_RABBITMQ_QUEUE`
      if not `austro.events`). Configure it for durable queues and consider
      message/policy mirroring for resilience.
- [ ] Set `AUSTRO_JWT_SECRET` and `AUSTRO_JWT_REFRESH_SECRET` to strong random
      values (`openssl rand -hex 32`). Startup fails fast on the dev placeholders.
- [ ] Decide live AI/delivery activation. To activate delivery set
      `AUSTRO_PUBLISH_BACKEND=generic-http`, `AUSTRO_PUBLISH_WEBHOOK_URL`,
      `AUSTRO_PUBLISH_TOKEN`; to activate AI set
      `AUSTRO_AI_BACKEND=openai-compatible`, `AUSTRO_AI_MODEL`,
      `AUSTRO_AI_BASE_URL`, `AUSTRO_AI_API_KEY`. Otherwise the offline stub
      defaults are used and no live external call is possible.
- [ ] Optional delivery reliability: `AUSTRO_PUBLISH_MAX_ATTEMPTS`,
      `AUSTRO_PUBLISH_RETRY_BACKOFF_BASE`, `AUSTRO_PUBLISH_RETRY_BACKOFF_MAX`
      (defaults: no retry, 500ms base, 30s cap). Set `MAX_ATTEMPTS >= 3` if the
      receiver is expected to honor `X-Idempotency`.
- [ ] Confirm `AUSTRO_ENV` is set to a production value the strict loader
      accepts in your pipeline.

## 2. First launch (smoke)

- [ ] Start the stack (`api`, `worker`, and dependencies) and wait for all
      healthchecks.
- [ ] Verify `GET /health/live` → `{"status":"ok"}` and
      `GET /health/ready` → `{"ready":true}`.
- [ ] Verify both startup parity logs list `composition ok`,
      `ai_backend=stub` (or the real backend), `publish_backend=stub` (or real),
      `usage_limit_per_workspace=<n>`.
- [ ] Verify the worker reports registering one consumer on the queue
      (`worker-started`, then a live `worker-message-received` from a seed).
- [ ] Seed one pipeline and confirm the cascade audit lines
      `research -> script -> review -> publish -> complete` appear, including
      `worker-pipeline-complete`.

## 3. Credentials and secrets hygiene (standing)

- [ ] Never commit `.env`, `*.pem`, `*.key`, `*.p12`, `*.pfx`, or real tokens;
      the repo ships `.env.example` placeholders only.
- [ ] Rotate `AUSTRO_JWT_*` and any boundary token on any suspected exposure;
      the design holds secrets in memory and surfaces status codes only.
- [ ] Review authz rules before granting any new endpoint; the middleware is
      deny-by-default — absence of a rule is a 403.

## 4. Backup and recovery

- [ ] Back up the PostgreSQL volume (`pg_dump`) on a schedule; restore must be
      exercised at least once before go-live.
- [ ] Redis is cache/memory only (no persistence configured) — treat it as
      rebuildable.
- [ ] RabbitMQ queue durability: keep `durable=true` declarations; if messages
      are critical across a broker restart ensure the deployment survives
      restarts before messages are ack'ed.

## 5. Incidents and error patterns to watch

- [ ] **Broker connection closes** (`AMQP connection closed` / missed heartbeat):
      expected and self-healing. Worker logs `worker-reconnected`; the sink
      redials silently. Alert only if reconnect keeps failing past the 30s cap.
- [ ] **Silent publish stall** → this failure mode no longer exists: the sink
      cannot silently die (ADR-014). If a cascade seems stuck, check the worker
      and postgres logs and the queue depth before suspecting the pipeline.
- [ ] **PostgreSQL `database system is in recovery mode`**: environmental
      (unclean shutdown → fsync-heavy recovery). Provision fast disk, keep
      steady-state writes low, and alert on recovery duration. The smoke test
      tolerates this; production monitoring should cover it.
- [ ] **RabbitMQ memory alarm** (`system_memory_high_watermark`): producers are
      flow-controlled under memory pressure. Alert on the alarm; provision broker
      memory well above the heartbeat of publisher traffic.
- [ ] **Module-proxy TLS timeouts** at build time: retryable; the build's retry
      loop and persistent module cache recover.

## 6. Scaling notes

- [ ] Worker concurrency is QoS 1 (one in-flight message). Scale by running more
      worker instances against the same queue; sticky/at-least-once semantics
      keep deliveries safe.
- [ ] API publishes are concurrency-safe by design (vended channels); no shared
      imported channel to protect.
- [ ] Per-workspace rate/usage guarding is at the Service/Gateway layers;
      horizontal API scale does not nullify it.

## 7. Known limits (before you promise ops)

- [ ] The real AI/delivery boundaries require the deferred composition wiring to
      be activated before they take runtime effect.
- [ ] No Prometheus/metrics export in-repo; structured logs are the contract.
- [ ] No broker mirroring/federation config in-repo; provision it at the broker.