package austro_os_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// isoFixture captures one workspace's hierarchy (department, team, employee).
type isoFixture struct {
	deptID  uuid.UUID
	teamID  uuid.UUID
	empID   uuid.UUID
	empName string
}

// prepareIsolationData seeds an identical hierarchy (department + team + AI
// employee) for both workspace A and workspace B via the admin connection.
// Restricted roles can only ever see their own workspace's rows under RLS.
func prepareIsolationData(t *testing.T, db *sql.DB) (a, b isoFixture) {
	t.Helper()
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	a = isoFixture{deptID: uuid.New(), teamID: uuid.New(), empID: uuid.New(), empName: "Emp A " + uuid.NewString()[:6]}
	b = isoFixture{deptID: uuid.New(), teamID: uuid.New(), empID: uuid.New(), empName: "Emp B " + uuid.NewString()[:6]}

	create := func(f *isoFixture, ws uuid.UUID) {
		deptName := "dept-" + f.deptID.String()[:8]
		teamName := "team-" + f.teamID.String()[:8]
		_, err := db.Exec(`INSERT INTO departments (id, name, workspace_id) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`,
			f.deptID, deptName, ws)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO teams (id, name, department_id) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`,
			f.teamID, teamName, f.deptID)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO ai_employees (id, name, role, team_id) VALUES ($1, $2, 'analyst', $3) ON CONFLICT (id) DO NOTHING`,
			f.empID, f.empName, f.teamID)
		require.NoError(t, err)
	}
	create(&a, wsA)
	create(&b, wsB)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM ai_employees WHERE id IN ($1,$2)`, a.empID, b.empID)
		_, _ = db.Exec(`DELETE FROM teams WHERE id IN ($1,$2)`, a.teamID, b.teamID)
		_, _ = db.Exec(`DELETE FROM departments WHERE id IN ($1,$2)`, a.deptID, b.deptID)
	})
	return a, b
}

// TestRLSPoliciesProper verifies that workspace isolation is enforced by real
// row-level security policies, never by disabling row security (ADR-007).
func TestRLSPoliciesProper(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	dataTables := []string{"workspaces", "departments", "teams", "ai_employees", "memory_embeddings"}
	for _, tbl := range dataTables {
		var n int
		require.NoError(t, admin.QueryRow(
			`SELECT count(*) FROM pg_policy WHERE polname='workspace_isolation_policy' AND polrelid=$1::regclass`,
			tbl).Scan(&n))
		require.Equal(t, 1, n, "expected workspace_isolation_policy on %s", tbl)

		var rlsEnabled bool
		require.NoError(t, admin.QueryRow(`SELECT relrowsecurity FROM pg_class WHERE oid=$1::regclass`, tbl).Scan(&rlsEnabled))
		require.True(t, rlsEnabled, "row-level security must be enabled on %s", tbl)
	}
}

// TestWorkspaceIsolation exercises the actual security boundary from the
// perspective of the two restricted database roles.
func TestWorkspaceIsolation(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	a, b := prepareIsolationData(t, admin)

	t.Run("A sees only A data", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		assertVisible(t, dbA, a, b, true)
	})

	t.Run("B sees only B data", func(t *testing.T) {
		dbB := connectRestricted(t, workspaceBRole)
		defer dbB.Close()
		setWorkspace(t, dbB, workspaceB)
		assertVisible(t, dbB, b, a, true)
	})

	t.Run("A cannot UPDATE B data", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		res, err := dbA.Exec(`UPDATE departments SET name='HACKED' WHERE id=$1`, b.deptID)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Zero(t, aff, "A must not update B's department")
	})

	t.Run("A cannot DELETE B data", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		res, err := dbA.Exec(`DELETE FROM departments WHERE id=$1`, b.deptID)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Zero(t, aff, "A must not delete B's department")
	})

	t.Run("B cannot DELETE A data", func(t *testing.T) {
		dbB := connectRestricted(t, workspaceBRole)
		defer dbB.Close()
		setWorkspace(t, dbB, workspaceB)
		res, err := dbB.Exec(`DELETE FROM ai_employees WHERE id=$1`, a.empID)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Zero(t, aff, "B must not delete A's employee")
	})

	t.Run("no workspace context is denied", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		var n int
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM departments`).Scan(&n))
		require.Zero(t, n, "without a workspace context no rows may be visible")
	})

	t.Run("A can update its own row", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		res, err := dbA.Exec(`UPDATE departments SET name='Dept A renamed' WHERE id=$1`, a.deptID)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Equal(t, int64(1), aff, "A must be able to update its own row")
	})

	t.Run("restricted roles are not privileged", func(t *testing.T) {
		for _, role := range []string{workspaceARole, workspaceBRole} {
			var isSuper bool
			var isBypass bool
			require.NoError(t, admin.QueryRow(`SELECT rolsuper FROM pg_roles WHERE rolname=$1`, role).Scan(&isSuper))
			require.NoError(t, admin.QueryRow(`SELECT rolbypassrls FROM pg_roles WHERE rolname=$1`, role).Scan(&isBypass))
			require.False(t, isSuper, "%s must not be a superuser", role)
			require.False(t, isBypass, "%s must not bypass RLS", role)
		}
	})
}

func connectRestricted(t *testing.T, role string) *sql.DB {
	t.Helper()
	db, err := connectAs(getEnv().postgresDSN, role)
	require.NoError(t, err)
	return db
}

func setWorkspace(t *testing.T, db *sql.DB, ws string) {
	t.Helper()
	_, err := db.Exec(`SELECT set_config('app.current_workspace', $1, false)`, ws)
	require.NoError(t, err)
}

// assertVisible checks that from the current session the fixture's own rows
// are visible and the other workspace's rows are not.
func assertVisible(t *testing.T, db *sql.DB, own, other isoFixture, expectOwn bool) {
	t.Helper()
	ownSeen, otherSeen := 0, 0
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM departments WHERE id=$1`, own.deptID).Scan(&ownSeen))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM departments WHERE id=$1`, other.deptID).Scan(&otherSeen))
	require.Equal(t, 1, ownSeen, "own department must be visible")
	require.Zero(t, otherSeen, "other workspace department must NOT be visible")

	empOwn, empOther := 0, 0
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM ai_employees WHERE id=$1`, own.empID).Scan(&empOwn))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM ai_employees WHERE id=$1`, other.empID).Scan(&empOther))
	require.Equal(t, 1, empOwn, "own employee must be visible")
	require.Zero(t, empOther, "other workspace employee must NOT be visible")
}
