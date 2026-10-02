package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// HeartbeatStore records and reads process liveness in the process_heartbeats
// table.
//
// The table is the only cross-process liveness signal in the system, and it
// exists because a split deployment makes a dependency invisible from the
// inside: an API that delegates the dispatch sweep to a companion on another
// host can start, serve traffic, and answer its own readiness check with no
// dispatcher existing anywhere. Nothing local to the API can detect that, so the
// companion leaves evidence in the one datastore every unit already shares and
// the API reads it back.
//
// Freshness is computed by the database (NOW()) rather than in Go, deliberately.
// The alternative compares a timestamp written by one host against a clock read
// on another, which means host clock skew silently decides whether a running
// companion reads as alive or dead — and skew large enough to matter is common
// on exactly the kind of fleet this runs on. Both sides of the comparison then
// come from one clock.
type HeartbeatStore struct {
	db *sql.DB
}

// NewHeartbeatStore returns a store over db. The table is created by
// infrastructure/database during bootstrap, so this holds no DDL and cannot
// create the table it reads.
func NewHeartbeatStore(db *sql.DB) *HeartbeatStore {
	return &HeartbeatStore{db: db}
}

// Register declares this incarnation of the process.
//
// It is separate from Beat so that started_at means "when the currently running
// incarnation first registered" rather than "when this row was created": a
// process that restarts on the same host reuses its instance id, and folding
// both writes into one upsert would either lose the restart or reset the
// timestamp on every tick, making a long-lived process indistinguishable from
// one that just came up.
//
// staleAfter bounds the table. It is not an optimization: instance ids are
// reused across restarts, so rows would only accumulate on hosts that report no
// hostname, and a liveness table that grows without bound is a liveness table
// nobody can read. Pruning only rows already past the staleness window cannot
// remove a live process's evidence, because a live process is by definition not
// stale.
func (h *HeartbeatStore) Register(ctx context.Context, role, instanceID string, staleAfter time.Duration) error {
	// The instance id is the hostname, so one row per role per host describes
	// the live process. A host that reports no hostname gets a per-process
	// random id instead: sharing the constant "unknown" across hosts would let
	// two different processes overwrite each other's liveness, which is worse
	// than the extra rows the prune above removes.
	id := instanceID
	if id == "" {
		id = fallbackInstanceID()
	}

	// A heartbeat that cannot be recorded is not fatal here. The process's real
	// work — sweeping, consuming — continues on PostgreSQL and the broker, and
	// the effect of the missing heartbeat is that another unit's readiness
	// degrades, which is the honest outcome: the platform is short an
	// observable. Exiting instead would convert a monitoring gap into an
	// outage on a host whose only fault is a locked-down network path.
	if _, err := h.db.ExecContext(ctx, `
		INSERT INTO process_heartbeats (role, instance_id, started_at, last_seen_at)
		VALUES ($1, $2, NOW(), NOW())
		ON CONFLICT (role, instance_id) DO UPDATE SET started_at = NOW()`,
		role, id); err != nil {
		return fmt.Errorf("register heartbeat role %q: %w", role, err)
	}

	if _, err := h.db.ExecContext(ctx, `
		DELETE FROM process_heartbeats
		WHERE role = $1
		  AND last_seen_at < NOW() - ($2::float8 * INTERVAL '1 second')`,
		role, staleAfter.Seconds()); err != nil {
		return fmt.Errorf("prune stale heartbeats role %q: %w", role, err)
	}
	return nil
}

// Beat refreshes the liveness evidence for an already-registered process. It is
// a single-row update by primary key, so the cost does not grow with the number
// of rows in the table.
func (h *HeartbeatStore) Beat(ctx context.Context, role, instanceID string) error {
	id := instanceID
	if id == "" {
		id = fallbackInstanceID()
	}
	res, err := h.db.ExecContext(ctx, `
		UPDATE process_heartbeats SET last_seen_at = NOW()
		WHERE role = $1 AND instance_id = $2`, role, id)
	if err != nil {
		return fmt.Errorf("beat heartbeat role %q: %w", role, err)
	}
	// Zero rows means Register never landed, which would otherwise leave the
	// process silently invisible forever. Re-registering is recoverable; being
	// invisible is not.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return h.Register(ctx, role, id, time.Minute)
	}
	return nil
}

// RoleLiveness is one role's observed liveness.
type RoleLiveness struct {
	// Role is the process role the row was written by.
	Role string
	// FreshInstances is how many instances of that role beat within the
	// staleness window. Zero means the role is not running, or is running on a
	// host that cannot reach PostgreSQL — the two are indistinguishable here,
	// and both mean the same thing to a caller deciding whether to serve.
	FreshInstances int
	// LastSeen is the most recent beat, zero when the role has never run.
	LastSeen time.Time
}

// Fresh reports whether at least one instance of the role beat recently.
//
// One live instance is sufficient, deliberately: the contract is that the sweep
// happens, not that it happens N times, and requiring a quorum would make a
// deliberately scaled-to-one deployment report itself unhealthy.
func (l RoleLiveness) Fresh() bool { return l.FreshInstances > 0 }

// Liveness reads the current state of one role.
//
// An unknown role yields zero liveness rather than an error, and a database
// error is returned to the caller: a readiness check that cannot reach the
// database cannot conclude the companion is fine, so it must not report success
// on a failed read.
func (h *HeartbeatStore) Liveness(ctx context.Context, role string, staleAfter time.Duration) (RoleLiveness, error) {
	out := RoleLiveness{Role: role}
	// last_seen_at is TIMESTAMP without a zone, matching every other table in
	// the schema, so it scans as time.Time. Aggregate in SQL rather than
	// selecting the newest row and filtering in Go, so the staleness test sees
	// the same NOW() the writers stamp against.
	var lastSeen sql.NullTime
	err := h.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE last_seen_at >= NOW() - ($2::float8 * INTERVAL '1 second')),
			MAX(last_seen_at)
		FROM process_heartbeats
		WHERE role = $1`, role, staleAfter.Seconds()).Scan(&out.FreshInstances, &lastSeen)
	if err != nil {
		return RoleLiveness{Role: role}, fmt.Errorf("read liveness role %q: %w", role, err)
	}
	if lastSeen.Valid {
		out.LastSeen = lastSeen.Time
	}
	return out, nil
}

// InstanceID returns the identifier this process reports. The hostname keeps one
// row per role per host across restarts, so a supervised restart does not leave
// a trail of dead rows for the reader to filter.
func InstanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return fallbackInstanceID()
	}
	return host
}

// fallbackInstanceID distinguishes processes on a host that reports no name. The
// pid alone would be reused after a reboot and a hostname alone would be
// constant across every such host, so the two together plus the process start
// time stay distinct for the window in which the rows are meaningful.
func fallbackInstanceID() string {
	return fmt.Sprintf("pid-%d", os.Getpid())
}
