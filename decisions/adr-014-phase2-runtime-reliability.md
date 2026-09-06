# ADR-014 — Phase 2 Runtime Reliability: Self-Healing Event Pipeline and Retrying Delivery

- **Status**: Accepted
- **Date**: Phase 2 runtime-reliability stage (Compose runtime, event-driven worker, release hardening)
- **Related**: ADR-010 (creator orchestration, message-driven loop), ADR-009 (publishing approvals), ADR-004 (AI gateway), ADR-013 (integration boundaries); CONSTITUTION.md P6 (Replaceability), P9 (Security by Design), P14 (Backward Compatibility); FINAL_RELEASE_AUDIT.md

## Context

The Phase 2 event pipeline is a durable RabbitMQ queue (`austro.events`) consumed
by the worker (`internal/worker`): a seeded `pipeline.research` message is
advanced through `script -> review -> publish -> complete`, with each stage
republishing the next `pipeline.<stage>` event on the same queue. During the
release-verification stage, live smoke testing of this cascade surfaced three
genuine reliability gaps, each reproduced in a running stack:

1. **The consumer could not survive a broker connection loss.** When RabbitMQ
   force-closed the worker's connection (this host does so on missed heartbeats),
   consumption stopped indefinitely. A supervised reconnect loop with exponential
   backoff was required.
2. **Persistent publishing used a start-up-only channel.** The worker and API
   built their event/audit sinks on a single AMQP channel created at process
   start and never reconnected. When the broker closed that connection the sink
   kept *silently failing* (`_ =` in the orchestration advance path), the
   downstream `pipeline.<stage>` event was never enqueued, and the cascade
   stalled mid-flight even though the consumer side had recovered.
3. **Delivery to an external endpoint was not resilient.** The generic HTTP
   publisher could not retry transient/5xx failures or carry an idempotency key,
   so a dropped call could not be safely replayed or deduplicated at the wire.

## Decision — supervised consumer reconnection (worker)

`internal/worker.Worker` now supervises its broker connection and deliveries
channel: on either closing, it drops the dead channel/connection, redials with
bounded exponential backoff (5s initial, 30s cap), re-declares the durable queue,
re-applies QoS, and re-registers the consumer. A `CloseBrokerConnection()` hook
exposes the recovery path for operator and test use. Regression:
`TestWorkerReconnectsAfterConnectionLoss`.

## Decision — self-supervised, concurrency-safe event sink (runtime entrypoints)

`infrastructure/rabbitmq` gains a reconnecting sink (`reconnect.go`):

- It owns a supervised AMQP connection that redials with bounded exponential
  backoff (500ms initial, 5s cap) whenever the broker closes it, and declares
  the durable queue on every (re)connect.
- It publishes on a **fresh channel per call**, closed after each publish.
  AMQP channels are single-goroutine by contract; the API publishes from
  concurrent HTTP handlers, so per-call channels are the concurrency-safe and
  leak-free choice.
- `cmd/worker` and the API entrypoint (`main.go`) wire all event/decision/audit
  sinks through this sink, replacing the static start-up channel. A
  `Close()/BreakConnection()` interface mirrors for publishing what the worker's
  reconnect hook does for consuming.
- The old static-channel constructor remains for scripts and controlled callers;
  runtime entrypoints never use it.

Regression: `TestEventSinkReconnectsAfterConnectionLoss` force-closes the sink's
connection, waits through the redial window, and asserts a subsequent publish
lands on the same durable queue.

## Decision — retry and idempotency for generic HTTP delivery (publishing boundary)

`internal/publish.GenericHTTPPublisher` (ADR-013) now retries transient/5xx
responses with exponential backoff up to a configurable attempt cap, never
retries 4xx or context cancellation, and sends an idempotency key
(`X-Idempotency`) derived from the workspace, publication id, and content hash,
or verbatim when the caller supplies `Publication.IdempotencyKey`. Failures
surface the HTTP status only — credentials are never exposed. The retry knob
default stays `1` (no retry) so the stub/default behavior is unchanged.
Regressions cover retry-then-success, 4xx no-retry, exhaustion, key stability
across attempts, verbatim explicit keys, and strict-config rejection of negative
values.

## Decision — the smoke test asserts the loop, not the disk

`TestWorkerAdvancesPipelineFromEventToComplete` keeps a fixed sequence
(seed -> publish one event -> assert `complete`/`done`) but its window widened
from 20s to 90s. On a healthy store the full cascade completes in under two
seconds (measured: 1.5s, 4.4s, 8.4s); on an overloaded host a single stage can
block tens of seconds behind a slow PostgreSQL fsync. The test proves the
end-to-end message loop and is not a disk-latency benchmark.

## Security / verification implications

- No new credentials, secrets, endpoints, or external calls (stub backends are
  the release default; the generic HTTP adapter stays CONFIGURATION_REQUIRED).
- No Phase 1 gate or workflow changes; additive only.
- Live full-suite pass at this decision: `ok austro-os/tests 88.843s` (exit 0),
  including `TestWorkerAdvancesPipelineFromEventToComplete` (4.42s),
  `TestWorkerReconnectsAfterConnectionLoss` (2.88s), and
  `TestEventSinkReconnectsAfterConnectionLoss` (8.26s); the running worker
  advanced seeded pipelines to `complete` with `outcome:success` audit lines.
- Configuration (`internal/config`) rejects negative retry/backoff values
  (canonical-env parity tests PASS).

## Consequences

- A broker blip no longer wedges consumption or publishing: both sides of the
  worker's cascade heal themselves with bounded backoff.
- Concurrent API publishers can no longer corrupt a shared AMQP channel or leak
  channels over time.
- External delivery is replay-safe: a dropped 5xx call is retried and the
  receiver can deduplicate by idempotency key.
- The observable failure signature changes from *silent stall* (no audit line,
  cascade dead) to *audited, self-healing* (reconnect lines, eventual advance).

## Out of scope (documented, not implemented)

- Flow-control/backpressure between queue depth and worker concurrency (QoS
  remains 1; a future scaling decision).
- Broker-side mirroring/federation; assumes the operator provisions RabbitMQ
  accordingly (see the operations checklist).
- Prometheus/metrics export; current observability is structured logs.