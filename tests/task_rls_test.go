package austro_os_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// grantTasksToRestrictedRoles grants the two restricted isolation roles DML on
// the tasks table so the RLS boundary can be exercised from their perspective.
// This is additive setup performed by the admin connection; Phase 1 helper
// grants are left untouched.
func grantTasksToRestrictedRoles(t *testing.T, admin *sql.DB, roles ...string) {
	t.Helper()
	for _, r := range roles {
		_, err := admin.Exec("GRANT SELECT, INSERT, UPDATE, DELETE ON tasks TO " + r)
		require.NoError(t, err)
	}
}

// TestTaskWorkspaceRLSIsolation proves tasks honour row-level security from the
// perspective of the restricted non-bypass roles, matching the Phase 1
// TestWorkspaceIsolation pattern but scoped to the tasks table.
func TestTaskWorkspaceRLSIsolation(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	grantTasksToRestrictedRoles(t, admin, workspaceARole, workspaceBRole)

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	// Seed one task for each workspace via the admin connection.
	taskA, taskB := uuid.New(), uuid.New()
	insA := func() error {
		_, e := admin.Exec(`INSERT INTO tasks (id, workspace_id, title, status, priority)
			VALUES ($1,$2,'A task','backlog','normal') ON CONFLICT (id) DO NOTHING`, taskA, wsA)
		return e
	}
	insB := func() error {
		_, e := admin.Exec(`INSERT INTO tasks (id, workspace_id, title, status, priority)
			VALUES ($1,$2,'B task','backlog','normal') ON CONFLICT (id) DO NOTHING`, taskB, wsB)
		return e
	}
	require.NoError(t, insA())
	require.NoError(t, insB())

	// Clean up the seeded tasks so repeated runs do not accumulate rows in the
	// shared workspaces (the RLS tests run after the lifecycle/count assertions).
	//
	// This goes through cleanupWithAdmin rather than the admin handle above:
	// t.Cleanup callbacks run after this function's deferred admin.Close(), so a
	// cleanup holding that handle silently deleted nothing and every run left two
	// more rows behind in the shared fixtures. cleanupWithAdmin opens its own
	// connection.
	cleanupWithAdmin(t, `DELETE FROM tasks WHERE id IN ($1, $2)`, taskA, taskB)

	t.Run("A sees only A tasks", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		aSeen, bSeen := 0, 0
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM tasks WHERE id=$1`, taskA).Scan(&aSeen))
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM tasks WHERE id=$1`, taskB).Scan(&bSeen))
		require.Equal(t, 1, aSeen, "A must see its own task")
		require.Zero(t, bSeen, "A must NOT see B's task")
	})

	t.Run("B sees only B tasks", func(t *testing.T) {
		dbB := connectRestricted(t, workspaceBRole)
		defer dbB.Close()
		setWorkspace(t, dbB, workspaceB)
		aSeen, bSeen := 0, 0
		require.NoError(t, dbB.QueryRow(`SELECT count(*) FROM tasks WHERE id=$1`, taskA).Scan(&aSeen))
		require.NoError(t, dbB.QueryRow(`SELECT count(*) FROM tasks WHERE id=$1`, taskB).Scan(&bSeen))
		require.Zero(t, aSeen, "B must NOT see A's task")
		require.Equal(t, 1, bSeen, "B must see its own task")
	})

	t.Run("A cannot update B tasks", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		res, err := dbA.Exec(`UPDATE tasks SET title='HACKED' WHERE id=$1`, taskB)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Zero(t, aff, "A must not update B's task")
	})

	t.Run("no workspace context reveals no tasks", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		var n int
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM tasks`).Scan(&n))
		require.Zero(t, n, "without a workspace context no tasks may be visible")
	})

	t.Run("A can update its own task", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		res, err := dbA.Exec(`UPDATE tasks SET title='A task renamed' WHERE id=$1`, taskA)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Equal(t, int64(1), aff, "A must be able to update its own task")
	})
}
