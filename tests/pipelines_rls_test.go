package austro_os_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// grantPipelineToRestrictedRoles grants the restricted isolation roles DML on
// the pipelines table (additive setup via the admin connection).
func grantPipelineToRestrictedRoles(t *testing.T, admin *sql.DB, roles ...string) {
	t.Helper()
	for _, r := range roles {
		_, err := admin.Exec("GRANT SELECT, INSERT, UPDATE, DELETE ON pipelines TO " + r)
		require.NoError(t, err)
	}
}

// seedPipelineRow inserts a deterministic pipelines row via the admin
// (bypassing RLS) so the isolation boundary can be observed from the restricted
// roles' perspective.
func seedPipelineRow(t *testing.T, admin *sql.DB, id, ws uuid.UUID, stage, status string) {
	t.Helper()
	_, err := admin.Exec(`INSERT INTO pipelines
		(id, workspace_id, stage, status, trace_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,NOW(),NOW())
		ON CONFLICT (id) DO NOTHING`,
		id, ws, stage, status, "trace-"+string(stage))
	require.NoError(t, err)
}

// TestPipeliningRLSPolicyEnabled verifies pipelines carries the standard
// workspace isolation policy and has row-level security enabled (added
// additively by the Phase 2 migration).
func TestPipeliningRLSPolicyEnabled(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	var n int
	require.NoError(t, admin.QueryRow(
		`SELECT count(*) FROM pg_policy WHERE polname='workspace_isolation_policy' AND polrelid='pipelines'::regclass`,
	).Scan(&n))
	require.Equal(t, 1, n, "expected workspace_isolation_policy on pipelines")

	var rlsEnabled bool
	require.NoError(t, admin.QueryRow(`SELECT relrowsecurity FROM pg_class WHERE oid='pipelines'::regclass`).Scan(&rlsEnabled))
	require.True(t, rlsEnabled, "row-level security must be enabled on pipelines")
}

// TestPipeliningWorkspaceRLSIsolation proves pipelines honour row-level security
// from the perspective of the restricted non-bypass roles, including update
// isolation.
func TestPipeliningWorkspaceRLSIsolation(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	grantPipelineToRestrictedRoles(t, admin, workspaceARole, workspaceBRole)

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	pipeA, pipeB := uuid.New(), uuid.New()
	seedPipelineRow(t, admin, pipeA, wsA, "script", "active")
	seedPipelineRow(t, admin, pipeB, wsB, "review", "awaiting_approval")
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM pipelines WHERE id IN ($1, $2)`, pipeA, pipeB)
	})

	t.Run("A sees only A pipelines", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		aSeen, bSeen := 0, 0
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM pipelines WHERE id=$1`, pipeA).Scan(&aSeen))
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM pipelines WHERE id=$1`, pipeB).Scan(&bSeen))
		require.Equal(t, 1, aSeen, "A must see its own pipeline")
		require.Zero(t, bSeen, "A must NOT see B's pipeline")
	})

	t.Run("B sees only B pipelines", func(t *testing.T) {
		dbB := connectRestricted(t, workspaceBRole)
		defer dbB.Close()
		setWorkspace(t, dbB, workspaceB)
		aSeen, bSeen := 0, 0
		require.NoError(t, dbB.QueryRow(`SELECT count(*) FROM pipelines WHERE id=$1`, pipeA).Scan(&aSeen))
		require.NoError(t, dbB.QueryRow(`SELECT count(*) FROM pipelines WHERE id=$1`, pipeB).Scan(&bSeen))
		require.Zero(t, aSeen, "B must NOT see A's pipeline")
		require.Equal(t, 1, bSeen, "B must see its own pipeline")
	})

	t.Run("A cannot update B pipelines", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		res, err := dbA.Exec(`UPDATE pipelines SET status='done' WHERE id=$1`, pipeB)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Zero(t, aff, "A must not update B's pipeline")
	})

	t.Run("no workspace context reveals no pipelines", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		var n int
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM pipelines`).Scan(&n))
		require.Zero(t, n, "without a workspace context no pipelines may be visible")
	})
}
