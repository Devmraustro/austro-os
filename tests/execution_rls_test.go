package austro_os_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// executionTables are the workspace-scoped AI execution aggregates. Every one
// of them must carry the standard workspace_isolation_policy and have RLS
// enabled and forced, or a tenant could read or mutate another tenant's
// executions through the API the runtime exposes.
var executionTables = []string{
	"ai_executions",
	"ai_execution_retries",
	"retry_dispatch_log",
	"ai_execution_dlq",
	"ai_tool_calls",
	"execution_contexts",
}

// skipIfNoDB skips the test when the live PostgreSQL fixture is unreachable,
// so the execution suite degrades gracefully on environments that only run
// unit tests. Every live test here is a security test, not a smoke test, so a
// skipped environment simply does not claim RLS coverage.
func skipIfNoDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := getEnv().AdminDB()
	if err != nil {
		t.Skipf("postgres unavailable, skipping RLS test: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("postgres unavailable, skipping RLS test: %v", err)
	}
	return db
}

// grantExecutionToRestrictedRoles grants the restricted isolation roles DML on
// the execution tables (additive setup via the admin connection).
func grantExecutionToRestrictedRoles(t *testing.T, admin *sql.DB, roles ...string) {
	t.Helper()
	for _, r := range roles {
		_, err := admin.Exec("GRANT SELECT, INSERT, UPDATE, DELETE ON ai_executions, ai_execution_retries, retry_dispatch_log, ai_execution_dlq, ai_tool_calls, execution_contexts TO " + r)
		require.NoError(t, err)
	}
}

// TestExecutionRLSPoliciesEnabled verifies every execution aggregate carries
// the standard workspace isolation policy and has row-level security enabled
// and forced.
func TestExecutionRLSPoliciesEnabled(t *testing.T) {
	admin := skipIfNoDB(t)
	defer admin.Close()

	for _, tbl := range executionTables {
		var n int
		require.NoError(t, admin.QueryRow(
			`SELECT count(*) FROM pg_policy WHERE polname='workspace_isolation_policy' AND polrelid=$1::regclass`,
			tbl).Scan(&n), "policy query for %s", tbl)
		require.Equal(t, 1, n, "expected workspace_isolation_policy on %s", tbl)

		var enabled, forced bool
		require.NoError(t, admin.QueryRow(`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid=$1::regclass`, tbl).Scan(&enabled, &forced), "rls flags for %s", tbl)
		require.True(t, enabled, "row-level security must be enabled on %s", tbl)
		require.True(t, forced, "row-level security must be FORCED on %s", tbl)
	}
}

// TestExecutionWorkspaceIsolation proves every execution aggregate honours
// row-level security from the perspective of the restricted non-bypass roles:
// a session bound to workspace A sees exactly its own row and never a row the
// admin seeded into workspace B.
func TestExecutionWorkspaceIsolation(t *testing.T) {
	admin := skipIfNoDB(t)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	grantExecutionToRestrictedRoles(t, admin, workspaceARole, workspaceBRole)

	a, b := prepareIsolationData(t, admin)
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	execA, execB := uuid.New(), uuid.New()
	for _, e := range []struct {
		id  uuid.UUID
		ws  uuid.UUID
		emp uuid.UUID
	}{
		{execA, wsA, a.empID},
		{execB, wsB, b.empID},
	} {
		_, err := admin.Exec(`INSERT INTO ai_executions (id, workspace_id, ai_employee_id, status, attempt, max_attempts, request_hash)
			VALUES ($1,$2,$3,'pending',0,3,'fixture-hash') ON CONFLICT (id) DO NOTHING`, e.id, e.ws, e.emp)
		require.NoError(t, err)
	}

	retryA, retryB := uuid.New(), uuid.New()
	for _, r := range []struct {
		id uuid.UUID
		ex uuid.UUID
		ws uuid.UUID
	}{
		{retryA, execA, wsA},
		{retryB, execB, wsB},
	} {
		_, err := admin.Exec(`INSERT INTO ai_execution_retries (id, execution_id, workspace_id, retry_seq, state)
			VALUES ($1,$2,$3,1,'pending') ON CONFLICT (id) DO NOTHING`, r.id, r.ex, r.ws)
		require.NoError(t, err)
	}

	dlqA, dlqB := uuid.New(), uuid.New()
	toolA, toolB := uuid.New(), uuid.New()
	ctxA, ctxB := uuid.New(), uuid.New()
	// Dispatch-log seeds are deterministic (fixed IDs): the assertions below
	// count the workspace's dispatch rows exactly, so a random ID per run would
	// accumulate rows across runs against a persistent database and flip the
	// exact-count assertions on the second run.
	dispatchA1, dispatchA2 := uuid.MustParse("66666666-0000-4000-8000-000000000001"), uuid.MustParse("66666666-0000-4000-8000-000000000002")
	dispatchB1, dispatchB2 := uuid.MustParse("66666666-0000-4000-8000-000000000003"), uuid.MustParse("66666666-0000-4000-8000-000000000004")
	seeds := []struct {
		table  string
		id     uuid.UUID
		ws     uuid.UUID
		execID uuid.UUID
		retry  uuid.UUID
	}{
		{"retry_dispatch_log", dispatchA1, wsA, uuid.Nil, retryA},
		{"retry_dispatch_log", dispatchA2, wsA, uuid.Nil, retryA},
		{"retry_dispatch_log", dispatchB1, wsB, uuid.Nil, retryB},
		{"retry_dispatch_log", dispatchB2, wsB, uuid.Nil, retryB},
		{"ai_execution_dlq", dlqA, wsA, execA, uuid.Nil},
		{"ai_execution_dlq", dlqB, wsB, execB, uuid.Nil},
		{"ai_tool_calls", toolA, wsA, execA, uuid.Nil},
		{"ai_tool_calls", toolB, wsB, execB, uuid.Nil},
		{"execution_contexts", ctxA, wsA, execA, uuid.Nil},
		{"execution_contexts", ctxB, wsB, execB, uuid.Nil},
	}
	for _, s := range seeds {
		var err error
		switch s.table {
		case "retry_dispatch_log":
			_, err = admin.Exec(`INSERT INTO retry_dispatch_log (id, workspace_id, retry_id, worker_id, generation, outcome)
				VALUES ($1,$2,$3,'w',0,'success') ON CONFLICT (id) DO NOTHING`, s.id, s.ws, s.retry)
		case "ai_execution_dlq":
			_, err = admin.Exec(`INSERT INTO ai_execution_dlq (id, workspace_id, execution_id, attempt, error_code, retryable)
				VALUES ($1,$2,$3,3,7,false) ON CONFLICT (id) DO NOTHING`, s.id, s.ws, s.execID)
		case "ai_tool_calls":
			_, err = admin.Exec(`INSERT INTO ai_tool_calls (id, workspace_id, execution_id, tool_name, status)
				VALUES ($1,$2,$3,'knowledge_search','completed') ON CONFLICT (id) DO NOTHING`, s.id, s.ws, s.execID)
		case "execution_contexts":
			_, err = admin.Exec(`INSERT INTO execution_contexts (id, workspace_id, execution_id, ai_employee_name, ai_employee_role, capabilities)
				VALUES ($1,$2,$3,'Emp X','analyst','{}') ON CONFLICT (id) DO NOTHING`, s.id, s.ws, s.execID)
		}
		require.NoError(t, err, "seed %s", s.table)
	}

	type rowPair struct {
		table string
		own   string
		other string
	}
	pairs := []rowPair{
		{"ai_executions", execA.String(), execB.String()},
		{"ai_execution_retries", retryA.String(), retryB.String()},
		{"retry_dispatch_log", "", ""},
		{"ai_execution_dlq", dlqA.String(), dlqB.String()},
		{"ai_tool_calls", toolA.String(), toolB.String()},
		{"execution_contexts", ctxA.String(), ctxB.String()},
	}

	dbA := connectRestricted(t, workspaceARole)
	defer dbA.Close()
	setWorkspace(t, dbA, workspaceA)

	for _, p := range pairs {
		if p.own == "" {
			continue // dispatch log ids are anonymous seeds; covered below via workspace column
		}
		var got int
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM `+p.table+` WHERE id=$1`, p.own).Scan(&got), "count own %s", p.table)
		require.Equal(t, 1, got, "workspace A role must see its own %s row", p.table)
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM `+p.table+` WHERE id=$1`, p.other).Scan(&got), "count other %s", p.table)
		require.Zero(t, got, "workspace A role must NOT see workspace B's %s row", p.table)
	}

	// The dispatch log has no stable cross-workspace id captured above, so the
	// same boundary is asserted by workspace count: A sees exactly its two
	// seeded logs and none of B's.
	var ownLogs, otherLogs int
	require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM retry_dispatch_log`).Scan(&ownLogs))
	require.Equal(t, 2, ownLogs, "workspace A role must see exactly its own dispatch log rows")
	require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM retry_dispatch_log WHERE workspace_id=$1`, wsB.String()).Scan(&otherLogs))
	require.Zero(t, otherLogs, "workspace A role must NOT see workspace B's dispatch log rows")
}
