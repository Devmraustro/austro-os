package austro_os_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/execution"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ReclaimStale resolves the outstanding retry row with a subquery restricted to
// state='pending'. A crash can leave the execution running with NO pending retry
// at all (the worker died before dispatch, or the row was already finalized),
// so the reclaim returns a NULL retry id. This file proves against the real
// PostgreSQL that such a reclaim is recovered exactly like any other: the
// execution goes back to pending, the generation has advanced, and the
// "at most one outstanding retry" invariant still holds afterwards.

// These identifiers are deliberately disjoint from the workspace/department
// pairs other suites seed (rls_runtime_role_test.go keeps rtWorkspaceA/B for
// exactly-count assertions, so sharing a workspace here would change their
// expected row counts).
const (
	reclaimWorkspaceA = "77777777-7777-4777-8777-777777777777"
	reclaimEmployeeA  = "88888888-8888-4888-8888-888888888888"
	reclaimDepartment = "99999999-9999-4999-8999-999999999999"
	reclaimTeam       = "abababab-abab-4bab-8bab-abababababab"
)

// reclaimFixture provisions the org chain and returns a pool connected as the
// runtime role: the role that actually performs recovery in production.
func reclaimFixture(t *testing.T) (*sql.DB, *sql.DB, uuid.UUID) {
	t.Helper()
	admin := skipIfNoDB(t)
	topo := topologyFixture(t)
	seedWorkspace(t, topo, reclaimWorkspaceA, "Reclaim A")

	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()
	_, err = owner.Exec(`INSERT INTO departments (id, workspace_id, name) VALUES ($1,$2,'Reclaim Dept')
		ON CONFLICT (id) DO NOTHING`, reclaimDepartment, reclaimWorkspaceA)
	require.NoError(t, err)
	_, err = owner.Exec(`INSERT INTO teams (id, department_id, name) VALUES ($1,$2,'Reclaim Team')
		ON CONFLICT (id) DO NOTHING`, reclaimTeam, reclaimDepartment)
	require.NoError(t, err)
	_, err = owner.Exec(`INSERT INTO ai_employees (id, team_id, name, role) VALUES ($1,$2,'Reclaim Employee','worker')
		ON CONFLICT (id) DO NOTHING`, reclaimEmployeeA, reclaimTeam)
	require.NoError(t, err)

	return admin, runtimeDB(t, topo), uuid.MustParse(reclaimWorkspaceA)
}

// seedRunningExecution writes an execution whose lease has already expired, in
// the exact state a crashed worker leaves behind.
func seedRunningExecution(t *testing.T, admin *sql.DB, ws, exec uuid.UUID) {
	t.Helper()
	_, err := admin.Exec(`DELETE FROM ai_executions WHERE id = $1`, exec)
	require.NoError(t, err)
	_, err = admin.Exec(`INSERT INTO ai_executions
		(id, workspace_id, ai_employee_id, status, attempt, max_attempts, request_hash,
		 lease_owner, lease_generation, lease_expires_at)
		VALUES ($1,$2,$3,'running',1,3,'reclaim-fixture','dead-worker',4,NOW() - INTERVAL '1 hour')`,
		exec, ws, reclaimEmployeeA)
	require.NoError(t, err)
}

// TestReclaimStaleRecoversCrashWithNoPendingRetry is the proof that the NULL
// retry id is benign: recovery reschedules the execution and the database is
// left with exactly one outstanding retry row, so the next dispatch can claim it
// without colliding with the partial unique index.
func TestReclaimStaleRecoversCrashWithNoPendingRetry(t *testing.T) {
	admin, rt, ws := reclaimFixture(t)
	store := postgres.NewExecutionStore(rt)
	ctx := context.Background()

	exec := uuid.New()
	seedRunningExecution(t, admin, ws, exec)
	// No retry row at all: this is the crash window the subquery cannot see.
	var retries int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM ai_execution_retries WHERE execution_id = $1`, exec).Scan(&retries))
	require.Zero(t, retries, "fixture precondition: the execution has no retry row")

	f, err := store.ReclaimStale(ctx, ws, exec, "recoverer-a", time.Minute)
	require.NoError(t, err, "an expired lease must be reclaimable")
	require.Equal(t, uuid.Nil, f.RetryID, "no pending retry row means no resolved retry id")
	require.Equal(t, int64(5), f.LeaseGeneration, "reclaim bumps the generation exactly once")
	require.Equal(t, "recoverer-a", f.LeaseOwner)

	// The dispatcher classifies a crash as worker_lease_expired and reschedules.
	next := time.Now().Add(2 * time.Second)
	require.NoError(t, store.Reschedule(ctx, f, next, f.Attempt+1))

	var status string
	var owner sql.NullString
	var expires sql.NullTime
	var nextAt sql.NullTime
	require.NoError(t, admin.QueryRow(
		`SELECT status, lease_owner, lease_expires_at, next_retry_at FROM ai_executions WHERE id = $1`, exec).
		Scan(&status, &owner, &expires, &nextAt))
	require.Equal(t, "pending", status, "recovery returns the execution to the queue")
	require.False(t, owner.Valid, "a released lease must clear its fingerprint")
	require.False(t, expires.Valid, "a released lease must clear its expiry")
	require.True(t, nextAt.Valid, "the rescheduled execution must carry its next retry time")

	// Exactly one outstanding retry, carrying the advanced sequence.
	rows, err := admin.Query(
		`SELECT retry_seq, state, worker_id FROM ai_execution_retries
		 WHERE execution_id = $1 AND state IN ('pending','claimed','dispatched')`, exec)
	require.NoError(t, err)
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var retrySeq int
		var state string
		var worker sql.NullString
		require.NoError(t, rows.Scan(&retrySeq, &state, &worker))
		seen++
		require.Equal(t, 2, retrySeq, "the retry sequence must advance past the crashed attempt")
		require.Equal(t, "pending", state)
		require.False(t, worker.Valid, "a fresh retry row must not inherit a worker fingerprint")
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 1, seen, "recovery must leave exactly one outstanding retry")
}

// TestReclaimStaleFinalizesTheClaimedRetry is the control case: with a
// 'claimed' retry row outstanding, the reclaim resolves that row, and the
// reschedule finalizes it (worker cleared) before inserting the next one --
// which is what makes the partial unique index satisfiable in both tests above.
func TestReclaimStaleFinalizesTheClaimedRetry(t *testing.T) {
	admin, rt, ws := reclaimFixture(t)
	store := postgres.NewExecutionStore(rt)
	ctx := context.Background()

	exec := uuid.New()
	seedRunningExecution(t, admin, ws, exec)
	old := uuid.New()
	_, err := admin.Exec(`INSERT INTO ai_execution_retries
		(id, execution_id, workspace_id, retry_seq, state, worker_id, generation)
		VALUES ($1,$2,$3,1,'claimed','dead-worker',4)`, old, exec, ws)
	require.NoError(t, err)

	f, err := store.ReclaimStale(ctx, ws, exec, "recoverer-b", time.Minute)
	require.NoError(t, err)
	// The reclaim only resolves rows in state='pending'; a 'claimed' row was
	// already dispatched to a worker, so the id comes back NULL here too. What
	// matters is that the reschedule finalizes it by execution, not by id.
	require.Equal(t, uuid.Nil, f.RetryID)

	require.NoError(t, store.Reschedule(ctx, f, time.Now().Add(time.Second), f.Attempt+1))

	var state string
	var outcome sql.NullString
	var worker sql.NullString
	require.NoError(t, admin.QueryRow(
		`SELECT state, outcome, worker_id FROM ai_execution_retries WHERE id = $1`, old).
		Scan(&state, &outcome, &worker))
	require.Equal(t, "processed", state, "the crashed attempt's retry must be finalized")
	require.Equal(t, "failed", outcome.String)
	require.False(t, worker.Valid, "a processed retry must carry no worker fingerprint")

	var outstanding int
	require.NoError(t, admin.QueryRow(
		`SELECT count(*) FROM ai_execution_retries WHERE execution_id = $1
		 AND state IN ('pending','claimed','dispatched')`, exec).Scan(&outstanding))
	require.Equal(t, 1, outstanding, "the next attempt is the only outstanding retry")
}

// TestRescheduleIsFencedAgainstThePreReclaimGeneration proves the recovery write
// is generation-fenced: a dispatcher that reclaims and then loses the row to a
// second reclaim cannot overwrite the newer owner's state.
func TestRescheduleIsFencedAgainstThePreReclaimGeneration(t *testing.T) {
	admin, rt, ws := reclaimFixture(t)
	store := postgres.NewExecutionStore(rt)
	ctx := context.Background()

	exec := uuid.New()
	seedRunningExecution(t, admin, ws, exec)

	first, err := store.ReclaimStale(ctx, ws, exec, "recoverer-c", time.Minute)
	require.NoError(t, err)
	// Age the lease again so a second recovery can take it over.
	_, err = admin.Exec(`UPDATE ai_executions SET lease_expires_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, exec)
	require.NoError(t, err)
	second, err := store.ReclaimStale(ctx, ws, exec, "recoverer-d", time.Minute)
	require.NoError(t, err)
	require.Greater(t, second.LeaseGeneration, first.LeaseGeneration)

	// The first recoverer is now stale and must be refused.
	err = store.Reschedule(ctx, first, time.Now().Add(time.Second), first.Attempt+1)
	require.ErrorIs(t, err, execution.ErrStaleLease, "a superseded generation must not reschedule")
	// The current owner still can.
	require.NoError(t, store.Reschedule(ctx, second, time.Now().Add(time.Second), second.Attempt+1))
}
