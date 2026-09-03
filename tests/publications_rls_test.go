package austro_os_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// grantPublishToRestrictedRoles grants the restricted isolation roles DML on
// the publications table (additive setup via the admin connection).
func grantPublishToRestrictedRoles(t *testing.T, admin *sql.DB, roles ...string) {
	t.Helper()
	for _, r := range roles {
		_, err := admin.Exec("GRANT SELECT, INSERT, UPDATE, DELETE ON publications TO " + r)
		require.NoError(t, err)
	}
}

// seedPublicationRow inserts a deterministic publications row via the admin
// (bypassing RLS) so the isolation boundary can be observed from the restricted
// roles' perspective.
func seedPublicationRow(t *testing.T, admin *sql.DB, id, ws uuid.UUID, status string) {
	t.Helper()
	_, err := admin.Exec(`INSERT INTO publications
		(id, workspace_id, title, body, platform, status, content_hash, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NOW(),NOW())
		ON CONFLICT (id) DO NOTHING`,
		id, ws, "A pub", "body", "stub", status, "hash-"+(string(status)))
	require.NoError(t, err)
}

// TestPublishingRLSPolicyEnabled verifies publications carries the standard
// workspace isolation policy and has row-level security enabled (added
// additively by the Phase 2 migration).
func TestPublishingRLSPolicyEnabled(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	var n int
	require.NoError(t, admin.QueryRow(
		`SELECT count(*) FROM pg_policy WHERE polname='workspace_isolation_policy' AND polrelid='publications'::regclass`,
	).Scan(&n))
	require.Equal(t, 1, n, "expected workspace_isolation_policy on publications")

	var rlsEnabled bool
	require.NoError(t, admin.QueryRow(`SELECT relrowsecurity FROM pg_class WHERE oid='publications'::regclass`).Scan(&rlsEnabled))
	require.True(t, rlsEnabled, "row-level security must be enabled on publications")
}

// TestPublishingWorkspaceRLSIsolation proves publications honour row-level
// security from the perspective of the restricted non-bypass roles, including
// status-filtered listing and update isolation.
func TestPublishingWorkspaceRLSIsolation(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	grantPublishToRestrictedRoles(t, admin, workspaceARole, workspaceBRole)

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	pubA, pubB := uuid.New(), uuid.New()
	seedPublicationRow(t, admin, pubA, wsA, "queued")
	seedPublicationRow(t, admin, pubB, wsB, "approved")
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM publications WHERE id IN ($1, $2)`, pubA, pubB)
	})

	t.Run("A sees only A publications", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		aSeen, bSeen := 0, 0
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM publications WHERE id=$1`, pubA).Scan(&aSeen))
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM publications WHERE id=$1`, pubB).Scan(&bSeen))
		require.Equal(t, 1, aSeen, "A must see its own publication")
		require.Zero(t, bSeen, "A must NOT see B's publication")
	})

	t.Run("B sees only B publications", func(t *testing.T) {
		dbB := connectRestricted(t, workspaceBRole)
		defer dbB.Close()
		setWorkspace(t, dbB, workspaceB)
		aSeen, bSeen := 0, 0
		require.NoError(t, dbB.QueryRow(`SELECT count(*) FROM publications WHERE id=$1`, pubA).Scan(&aSeen))
		require.NoError(t, dbB.QueryRow(`SELECT count(*) FROM publications WHERE id=$1`, pubB).Scan(&bSeen))
		require.Zero(t, aSeen, "B must NOT see A's publication")
		require.Equal(t, 1, bSeen, "B must see its own publication")
	})

	t.Run("status filter is workspace-scoped", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		var approvedSeen int
		require.NoError(t, dbA.QueryRow(
			`SELECT count(*) FROM publications WHERE status='approved'`).Scan(&approvedSeen))
		require.Zero(t, approvedSeen, "A must not see B's approved publication")
	})

	t.Run("A cannot update B publications", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		res, err := dbA.Exec(`UPDATE publications SET status='published' WHERE id=$1`, pubB)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Zero(t, aff, "A must not update B's publication")
	})

	t.Run("no workspace context reveals no publications", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		var n int
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM publications`).Scan(&n))
		require.Zero(t, n, "without a workspace context no publications may be visible")
	})
}
