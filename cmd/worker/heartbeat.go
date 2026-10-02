package main

import (
	"context"
	"database/sql"
	"time"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/config"
	logger "austro-os/internal/log"
)

// startHeartbeat registers this process in process_heartbeats and keeps its row
// fresh, returning the stop function that ends the loop.
//
// The worker beats for the same reason the dispatcher does: an API in a split
// deployment is the process that has to report this one as present or absent,
// and it can only do that if the consumer leaves evidence. Without the worker's
// beat, a queue with no consumer is indistinguishable from a queue being drained
// — the execution stays 'pending' at the API while nothing looks wrong.
//
// A heartbeat that cannot be recorded is logged, not fatal. The consumer's real
// work is on the broker, and it continues regardless. Making a lost heartbeat
// fatal would turn a monitoring gap into an outage, and it would also be a
// backwards coupling: the worker's liveness signal would depend on the
// database, which the consumer is otherwise able to survive losing. The honest
// consequence is the one that already exists — an API that declared a worker
// reports it absent, and the executions stuck 'pending' are visible in
// PostgreSQL where an operator will actually look.
func startHeartbeat(cfg *config.Config, db *sql.DB) func() {
	store := postgres.NewHeartbeatStore(db)
	instanceID := postgres.InstanceID()
	ctx, cancel := context.WithCancel(context.Background())

	// A registration failure is retried on the tick below rather than returned,
	// so a database that is still coming up when the process starts is not
	// permanently invisible. Beat re-registers when its row is missing, so the
	// two paths share one recovery mechanism.
	if err := store.Register(ctx, config.ProcessRoleWorker, instanceID, cfg.HeartbeatStaleAfter); err != nil {
		logger.NewEntry("heartbeat-register-failed").
			SetLevel("warn").
			With("role", config.ProcessRoleWorker).
			WithError(err).
			Log()
	} else {
		logger.NewEntry("heartbeat-registered").
			With("role", config.ProcessRoleWorker).
			With("instance", instanceID).
			Log()
	}

	go func() {
		ticker := time.NewTicker(cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := store.Beat(ctx, config.ProcessRoleWorker, instanceID); err != nil {
					logger.NewEntry("heartbeat-beat-failed").
						SetLevel("warn").
						With("role", config.ProcessRoleWorker).
						WithError(err).
						Log()
				}
			}
		}
	}()

	return cancel
}
