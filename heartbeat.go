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
// It is used by the two roles that other processes depend on — the dispatcher
// and the worker — and by neither of the roles that do not. The API does not
// beat, because nothing depends on the API's liveness through this table: the
// platform supervises the site process directly, and a heartbeat from it would
// add a row that only ever confirms what the platform already enforces.
//
// A heartbeat that cannot be recorded is logged, not fatal. The process's real
// work is elsewhere — sweeping, consuming — and it continues on PostgreSQL and
// the broker regardless. Treating a lost heartbeat as fatal would convert a
// monitoring gap into an outage on a host whose only fault is a restricted
// network path, which trades a visible degradation for an invisible one. The
// honest consequence is the one that already exists: any API that declared
// this companion reports it absent.
func startHeartbeat(cfg *config.Config, db *sql.DB, role string) func() {
	store := postgres.NewHeartbeatStore(db)
	instanceID := postgres.InstanceID()
	ctx, cancel := context.WithCancel(context.Background())

	// A registration failure is retried on the tick below rather than returned,
	// so a database that is still coming up when the process starts is not
	// permanently invisible. Beat re-registers when its row is missing, so the
	// two paths share one recovery mechanism.
	if err := store.Register(ctx, role, instanceID, cfg.HeartbeatStaleAfter); err != nil {
		logger.NewEntry("heartbeat-register-failed").
			SetLevel("warn").
			With("role", role).
			WithError(err).
			Log()
	} else {
		logger.NewEntry("heartbeat-registered").With("role", role).With("instance", instanceID).Log()
	}

	go func() {
		ticker := time.NewTicker(cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := store.Beat(ctx, role, instanceID); err != nil {
					logger.NewEntry("heartbeat-beat-failed").
						SetLevel("warn").
						With("role", role).
						WithError(err).
						Log()
				}
			}
		}
	}()

	return cancel
}
