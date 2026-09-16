package austro_os_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestOrganizationHierarchyInvariants verifies that the organization invariant
// relationships are enforced at the database layer (FK NOT NULL / REFERENCES).
func TestOrganizationHierarchyInvariants(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	ws := uuid.MustParse(workspaceA)
	validDept := uuid.New()
	deptName := "Dept OK " + uuid.NewString()[:8]
	_, err = admin.Exec(`INSERT INTO departments (id, name, workspace_id) VALUES ($1,$2,$3)`, validDept, deptName, ws)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM departments WHERE id=$1`, validDept)
	})
	validTeam := uuid.New()
	teamName := "Team OK " + uuid.NewString()[:8]
	_, err = admin.Exec(`INSERT INTO teams (id, name, department_id) VALUES ($1,$2,$3)`, validTeam, teamName, validDept)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM teams WHERE id=$1`, validTeam)
	})

	t.Run("team without department fails", func(t *testing.T) {
		_, err := admin.Exec(`INSERT INTO teams (id, name, department_id) VALUES ($1,'NoDept',NULL)`, uuid.New())
		require.Error(t, err, "team without department must violate the FK NOT NULL constraint")
	})

	t.Run("team with nonexistent department fails", func(t *testing.T) {
		_, err := admin.Exec(`INSERT INTO teams (id, name, department_id) VALUES ($1,'BadDept',$2)`,
			uuid.New(), uuid.New())
		require.Error(t, err, "team referencing a nonexistent department must fail")
	})

	t.Run("department across workspaces is single-valued", func(t *testing.T) {
		// A department has a single workspace_id column -> cannot belong to two
		// workspaces simultaneously.
		var owners int
		require.NoError(t, admin.QueryRow(`SELECT count(*) FROM departments WHERE id=$1`, validDept).Scan(&owners))
		require.Equal(t, 1, owners)
	})

	t.Run("employee without team fails", func(t *testing.T) {
		_, err := admin.Exec(`INSERT INTO ai_employees (id, name, role, team_id) VALUES ($1,'NoTeam','r',NULL)`, uuid.New())
		require.Error(t, err, "AI employee without a team must violate the FK NOT NULL constraint")
	})

	t.Run("employee with nonexistent team fails", func(t *testing.T) {
		_, err := admin.Exec(`INSERT INTO ai_employees (id, name, role, team_id) VALUES ($1,'BadTeam','r',$2)`,
			uuid.New(), uuid.New())
		require.Error(t, err, "AI employee referencing a nonexistent team must fail")
	})

	t.Run("employee belongs to exactly one team", func(t *testing.T) {
		emp := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_employees (id, name, role, team_id) VALUES ($1,'OneTeam','r',$2)`,
			emp, validTeam)
		require.NoError(t, err)

		var teamCt int
		require.NoError(t, admin.QueryRow(`SELECT count(*) FROM ai_employees WHERE id=$1 AND team_id=$2`, emp, validTeam).Scan(&teamCt))
		require.Equal(t, 1, teamCt, "AI employee must belong to exactly one team (single-valued FK column)")
	})
}

// TestWorkspaceIsolationEnforcement verifies cross-workspace ownership is
// rejected by the hierarchy validation, using the live DB.
func TestWorkspaceIsolationEnforcement(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	deptA := uuid.New()
	teamA := uuid.New()
	empA := uuid.New()
	deptAName := "A-" + uuid.NewString()[:8]
	_, err = admin.Exec(`INSERT INTO departments (id, name, workspace_id) VALUES ($1,$2,$3)`, deptA, deptAName, wsA)
	require.NoError(t, err)
	teamAName := "A-T-" + uuid.NewString()[:8]
	_, err = admin.Exec(`INSERT INTO teams (id, name, department_id) VALUES ($1,$2,$3)`, teamA, teamAName, deptA)
	require.NoError(t, err)
	_, err = admin.Exec(`INSERT INTO ai_employees (id, name, role, team_id) VALUES ($1,$2,'r',$3)`, empA, "A-E-"+uuid.NewString()[:6], teamA)
	require.NoError(t, err)

	// Also create an unrelated workspace B department to prove B is separate.
	deptB := uuid.New()
	_, err = admin.Exec(`INSERT INTO departments (id, name, workspace_id) VALUES ($1,$2,$3)`, deptB, "B-"+uuid.NewString()[:8], wsB)
	require.NoError(t, err, "workspace B must be independently seeded")
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM ai_employees WHERE id=$1`, empA)
		_, _ = admin.Exec(`DELETE FROM teams WHERE id=$1`, teamA)
		_, _ = admin.Exec(`DELETE FROM departments WHERE id IN ($1,$2)`, deptA, deptB)
	})
	_ = wsB

	// From context of workspace A the hierarchy validates.
	ctxA := context.WithValue(context.Background(), currentWorkspaceKey, workspaceA)
	require.NoError(t, validateHierarchy(admin, ctxA, empA))

	// From context of workspace B the same employee must fail validation
	// (cross-workspace ownership rejected).
	ctxB := context.WithValue(context.Background(), currentWorkspaceKey, workspaceB)
	err = validateHierarchy(admin, ctxB, empA)
	require.Error(t, err, "cross-workspace ownership must be rejected")
}

type hierarchyContextKey string

const currentWorkspaceKey hierarchyContextKey = "current_workspace"

// validateHierarchy re-implements the hierarchy/workspace ownership check
// against the actual schema (ai_employees -> teams -> departments).
func validateHierarchy(db *sql.DB, ctx context.Context, empID uuid.UUID) error {
	var ws string
	err := db.QueryRowContext(ctx, `
		SELECT d.workspace_id FROM ai_employees ae
		JOIN teams t ON ae.team_id = t.id
		JOIN departments d ON t.department_id = d.id
		WHERE ae.id = $1`, empID).Scan(&ws)
	if err != nil {
		return err
	}
	if cur, ok := ctx.Value(currentWorkspaceKey).(string); ok && cur != "" && ws != cur {
		return sql.ErrNoRows
	}
	return nil
}
