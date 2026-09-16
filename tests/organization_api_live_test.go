package austro_os_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Live tests for the organizational hierarchy: Departments → Teams → AI Employees.
// These prove the composition in main.go works: routes registered, authorizer
// runs before them, handler reaches real Postgres store through unprivileged
// runtime role, workspace scope deterministic, validation strict, audit recorded.

// departmentLive mirrors the API envelope.
type departmentLive struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type departmentPageLive struct {
	Departments []departmentLive `json:"departments"`
	NextCursor  string           `json:"next_cursor"`
	Limit       int              `json:"limit"`
}

type teamLive struct {
	ID           string `json:"id"`
	DepartmentID string `json:"department_id"`
	WorkspaceID  string `json:"workspace_id"`
	Name         string `json:"name"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type teamPageLive struct {
	Teams      []teamLive `json:"teams"`
	NextCursor string     `json:"next_cursor"`
	Limit      int        `json:"limit"`
}

type aiEmployeeLive struct {
	ID            string   `json:"id"`
	TeamID        string   `json:"team_id"`
	DepartmentID  string   `json:"department_id"`
	WorkspaceID   string   `json:"workspace_id"`
	Name          string   `json:"name"`
	Role          string   `json:"role"`
	Capabilities  []string `json:"capabilities"`
	CurrentTaskID string   `json:"current_task_id,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

type aiEmployeePageLive struct {
	Employees  []aiEmployeeLive `json:"ai_employees"`
	NextCursor string           `json:"next_cursor"`
	Limit      int              `json:"limit"`
}

func createDepartmentLive(t *testing.T, token, name string) departmentLive {
	t.Helper()
	status, body := authJSONRaw(t, http.MethodPost, "/departments",
		fmt.Sprintf(`{"name":%q}`, name), token)
	require.Equal(t, http.StatusCreated, status, "department creation must succeed: %s", body)
	var out departmentLive
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, name, out.Name)
	require.NotEmpty(t, out.WorkspaceID)
	return out
}

func createTeamLive(t *testing.T, token, name, departmentID string) teamLive {
	t.Helper()
	status, body := authJSONRaw(t, http.MethodPost, "/teams",
		fmt.Sprintf(`{"name":%q,"department_id":%q}`, name, departmentID), token)
	require.Equal(t, http.StatusCreated, status, "team creation must succeed: %s", body)
	var out teamLive
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, name, out.Name)
	require.Equal(t, departmentID, out.DepartmentID)
	return out
}

func createAIEmployeeLive(t *testing.T, token, name, role, teamID string, caps []string) aiEmployeeLive {
	t.Helper()
	capsJSON, _ := json.Marshal(caps)
	status, body := authJSONRaw(t, http.MethodPost, "/ai-employees",
		fmt.Sprintf(`{"name":%q,"role":%q,"team_id":%q,"capabilities":%s}`, name, role, teamID, string(capsJSON)), token)
	require.Equal(t, http.StatusCreated, status, "ai employee creation must succeed: %s", body)
	var out aiEmployeeLive
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, name, out.Name)
	require.Equal(t, role, out.Role)
	require.Equal(t, teamID, out.TeamID)
	return out
}

func createTaskForAssignLive(t *testing.T, token, title string) string {
	t.Helper()
	status, body := authJSONRaw(t, http.MethodPost, "/tasks",
		fmt.Sprintf(`{"title":%q}`, title), token)
	require.Equal(t, http.StatusCreated, status, "task creation for assignment must succeed: %s", body)
	var out taskLive
	require.NoError(t, json.Unmarshal(body, &out))
	return out.ID
}

func TestDepartmentLiveJourney(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	marker := "dept-" + uuid.NewString()[:8]

	created := createDepartmentLive(t, tokens.adminA, marker)

	t.Run("read back", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/departments/"+created.ID, "", tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var got departmentLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, created.ID, got.ID)
		require.Equal(t, created.Name, got.Name)
		require.Equal(t, created.WorkspaceID, got.WorkspaceID)
	})

	t.Run("appears in listing", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/departments?limit=200", "", tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var page departmentPageLive
		require.NoError(t, json.Unmarshal(body, &page))
		require.LessOrEqual(t, len(page.Departments), 200)
		require.Equal(t, 200, page.Limit)
		found := false
		for _, d := range page.Departments {
			require.Equal(t, created.WorkspaceID, d.WorkspaceID, "listing must be workspace-scoped")
			if d.ID == created.ID {
				found = true
			}
		}
		require.True(t, found)
	})

	t.Run("member can read but not create", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/departments/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		status, _ = authJSONRaw(t, http.MethodPost, "/departments", `{"name":"member-should-not"}`, tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)
	})

	t.Run("update renames", func(t *testing.T) {
		newName := marker + "-renamed"
		status, body := authJSONRaw(t, http.MethodPatch, "/departments/"+created.ID,
			fmt.Sprintf(`{"name":%q}`, newName), tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var got departmentLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, newName, got.Name)
		require.Equal(t, created.CreatedAt, got.CreatedAt)
	})

	t.Run("duplicate name is conflict", func(t *testing.T) {
		dupName := "dup-" + uuid.NewString()[:8]
		d1 := createDepartmentLive(t, tokens.adminA, dupName)
		status, body := authJSONRaw(t, http.MethodPost, "/departments",
			fmt.Sprintf(`{"name":%q}`, dupName), tokens.adminA)
		require.Equal(t, http.StatusConflict, status, "duplicate department name must be 409: %s", body)
		// cleanup dup
		status, _ = authJSONRaw(t, http.MethodDelete, "/departments/"+d1.ID, "", tokens.adminA)
		require.Equal(t, http.StatusNoContent, status)
	})

	t.Run("audit trail recorded", func(t *testing.T) {
		founder := auditFounderToken(t)
		for _, evType := range []string{"department.create", "department.update"} {
			status, body := authJSONRaw(t, http.MethodGet, "/audit/events?event_type="+evType+"&limit=200", "", founder)
			require.Equal(t, http.StatusOK, status, body)
			var page auditPage
			require.NoError(t, json.Unmarshal(body, &page))
			require.NotEmpty(t, page.Events, "%s must be audited", evType)
			for _, ev := range page.Events {
				require.Equal(t, evType, ev.EventType)
				require.Equal(t, "user", ev.ActorType)
			}
		}
	})
}

func TestTeamLiveJourney(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	marker := "team-" + uuid.NewString()[:8]
	dept := createDepartmentLive(t, tokens.adminA, "dept-for-"+marker)

	t.Run("create team in department", func(t *testing.T) {
		created := createTeamLive(t, tokens.adminA, marker, dept.ID)
		require.Equal(t, dept.ID, created.DepartmentID)
		require.Equal(t, dept.WorkspaceID, created.WorkspaceID)

		status, body := authJSONRaw(t, http.MethodGet, "/teams/"+created.ID, "", tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var got teamLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, created.ID, got.ID)
		require.Equal(t, dept.ID, got.DepartmentID)
	})

	t.Run("list filtered by department", func(t *testing.T) {
		m1 := "filter-" + uuid.NewString()[:8]
		m2 := "filter-" + uuid.NewString()[:8]
		dept2 := createDepartmentLive(t, tokens.adminA, "dept2-"+marker)
		team1 := createTeamLive(t, tokens.adminA, m1, dept.ID)
		team2 := createTeamLive(t, tokens.adminA, m2, dept2.ID)

		status, body := authJSONRaw(t, http.MethodGet, "/teams?department_id="+dept.ID+"&limit=200", "", tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var page teamPageLive
		require.NoError(t, json.Unmarshal(body, &page))
		found1 := false
		found2 := false
		for _, tm := range page.Teams {
			if tm.ID == team1.ID {
				found1 = true
			}
			if tm.ID == team2.ID {
				found2 = true
			}
			require.Equal(t, dept.WorkspaceID, tm.WorkspaceID)
		}
		require.True(t, found1, "filtered listing must contain team from requested department")
		require.False(t, found2, "filtered listing must not contain team from other department")

		// cleanup
		authJSONRaw(t, http.MethodDelete, "/teams/"+team1.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team2.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/departments/"+dept2.ID, "", tokens.adminA)
	})

	t.Run("move team to another department in same workspace", func(t *testing.T) {
		dept2 := createDepartmentLive(t, tokens.adminA, "move-target-"+marker)
		team := createTeamLive(t, tokens.adminA, "move-"+marker, dept.ID)
		status, body := authJSONRaw(t, http.MethodPatch, "/teams/"+team.ID,
			fmt.Sprintf(`{"department_id":%q}`, dept2.ID), tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var got teamLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, dept2.ID, got.DepartmentID)

		// cleanup
		authJSONRaw(t, http.MethodDelete, "/teams/"+team.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/departments/"+dept2.ID, "", tokens.adminA)
	})

	t.Run("member read-only for teams", func(t *testing.T) {
		team := createTeamLive(t, tokens.adminA, "ro-"+marker, dept.ID)
		status, body := authJSONRaw(t, http.MethodGet, "/teams/"+team.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		status, _ = authJSONRaw(t, http.MethodPost, "/teams", fmt.Sprintf(`{"name":"x","department_id":%q}`, dept.ID), tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team.ID, "", tokens.adminA)
	})

	t.Run("audit recorded", func(t *testing.T) {
		founder := auditFounderToken(t)
		status, body := authJSONRaw(t, http.MethodGet, "/audit/events?event_type=team.create&limit=200", "", founder)
		require.Equal(t, http.StatusOK, status, body)
		var page auditPage
		require.NoError(t, json.Unmarshal(body, &page))
		require.NotEmpty(t, page.Events)
	})
}

func TestAIEmployeeLiveJourney(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	marker := "emp-" + uuid.NewString()[:8]
	dept := createDepartmentLive(t, tokens.adminA, "dept-emp-"+marker)
	team := createTeamLive(t, tokens.adminA, "team-emp-"+marker, dept.ID)

	created := createAIEmployeeLive(t, tokens.adminA, marker, "engineer", team.ID, []string{"coding", "review"})

	t.Run("hierarchy fields are denormalized", func(t *testing.T) {
		require.Equal(t, team.ID, created.TeamID)
		require.Equal(t, dept.ID, created.DepartmentID)
		require.Equal(t, dept.WorkspaceID, created.WorkspaceID)
	})

	t.Run("read back", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/ai-employees/"+created.ID, "", tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var got aiEmployeeLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, created.ID, got.ID)
		require.Equal(t, team.ID, got.TeamID)
		require.Equal(t, dept.ID, got.DepartmentID)
		require.Equal(t, dept.WorkspaceID, got.WorkspaceID)
		require.Equal(t, "engineer", got.Role)
	})

	t.Run("appears in listing and filtered by team", func(t *testing.T) {
		team2 := createTeamLive(t, tokens.adminA, "team2-"+marker, dept.ID)
		emp2 := createAIEmployeeLive(t, tokens.adminA, marker+"-2", "designer", team2.ID, []string{"design"})

		status, body := authJSONRaw(t, http.MethodGet, "/ai-employees?limit=200", "", tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var page aiEmployeePageLive
		require.NoError(t, json.Unmarshal(body, &page))
		found := false
		for _, e := range page.Employees {
			require.Equal(t, dept.WorkspaceID, e.WorkspaceID)
			if e.ID == created.ID {
				found = true
			}
		}
		require.True(t, found)

		status, body = authJSONRaw(t, http.MethodGet, "/ai-employees?team_id="+team.ID+"&limit=200", "", tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		require.NoError(t, json.Unmarshal(body, &page))
		found1 := false
		found2 := false
		for _, e := range page.Employees {
			if e.ID == created.ID {
				found1 = true
			}
			if e.ID == emp2.ID {
				found2 = true
			}
		}
		require.True(t, found1)
		require.False(t, found2)

		// cleanup second
		authJSONRaw(t, http.MethodDelete, "/ai-employees/"+emp2.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team2.ID, "", tokens.adminA)
	})

	t.Run("update role and capabilities and move", func(t *testing.T) {
		team2 := createTeamLive(t, tokens.adminA, "move-emp-"+marker, dept.ID)
		status, body := authJSONRaw(t, http.MethodPatch, "/ai-employees/"+created.ID,
			fmt.Sprintf(`{"role":"senior engineer","capabilities":["coding","review","mentoring"],"team_id":%q}`, team2.ID), tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var got aiEmployeeLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "senior engineer", got.Role)
		require.Equal(t, team2.ID, got.TeamID)
		require.ElementsMatch(t, []string{"coding", "review", "mentoring"}, got.Capabilities)

		// move back for further tests
		authJSONRaw(t, http.MethodPatch, "/ai-employees/"+created.ID,
			fmt.Sprintf(`{"team_id":%q}`, team.ID), tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team2.ID, "", tokens.adminA)
	})

	t.Run("assign task", func(t *testing.T) {
		taskID := createTaskForAssignLive(t, tokens.adminA, "assign-"+marker)
		status, body := authJSONRaw(t, http.MethodPatch, "/ai-employees/"+created.ID,
			fmt.Sprintf(`{"current_task_id":%q}`, taskID), tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var got aiEmployeeLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, taskID, got.CurrentTaskID)

		// clear assignment
		status, body = authJSONRaw(t, http.MethodPatch, "/ai-employees/"+created.ID,
			`{"current_task_id":""}`, tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		require.NoError(t, json.Unmarshal(body, &got))
		require.Empty(t, got.CurrentTaskID)
	})

	t.Run("member read-only", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/ai-employees/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		status, _ = authJSONRaw(t, http.MethodPost, "/ai-employees",
			fmt.Sprintf(`{"name":"x","role":"y","team_id":%q}`, team.ID), tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)
	})

	t.Run("audit recorded", func(t *testing.T) {
		founder := auditFounderToken(t)
		status, body := authJSONRaw(t, http.MethodGet, "/audit/events?event_type=ai_employee.create&limit=200", "", founder)
		require.Equal(t, http.StatusOK, status, body)
		var page auditPage
		require.NoError(t, json.Unmarshal(body, &page))
		require.NotEmpty(t, page.Events)
	})

	// cleanup
	t.Cleanup(func() {
		authJSONRaw(t, http.MethodDelete, "/ai-employees/"+created.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/departments/"+dept.ID, "", tokens.adminA)
	})
}

func TestOrganizationLiveAuthorization(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	founder := auditFounderToken(t)
	dept := createDepartmentLive(t, tokens.adminA, "authz-dept-"+uuid.NewString()[:8])
	team := createTeamLive(t, tokens.adminA, "authz-team-"+uuid.NewString()[:8], dept.ID)
	emp := createAIEmployeeLive(t, tokens.adminA, "authz-emp-"+uuid.NewString()[:8], "role", team.ID, nil)

	t.Run("anonymous is forbidden, invalid token is unauthorized", func(t *testing.T) {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/departments", ""},
			{http.MethodPost, "/departments", `{"name":"x"}`},
			{http.MethodGet, "/departments/" + dept.ID, ""},
			{http.MethodGet, "/teams", ""},
			{http.MethodPost, "/teams", fmt.Sprintf(`{"name":"x","department_id":%q}`, dept.ID)},
			{http.MethodGet, "/teams/" + team.ID, ""},
			{http.MethodGet, "/ai-employees", ""},
			{http.MethodPost, "/ai-employees", fmt.Sprintf(`{"name":"x","role":"y","team_id":%q}`, team.ID)},
			{http.MethodGet, "/ai-employees/" + emp.ID, ""},
		} {
			status, body := authJSONRaw(t, tc.method, tc.path, tc.body, "")
			require.Equal(t, http.StatusForbidden, status, "%s %s without token must be 403: %s", tc.method, tc.path, body)
			status, body = authJSONRaw(t, tc.method, tc.path, tc.body, "garbage-token")
			require.Equal(t, http.StatusUnauthorized, status, "%s %s invalid token must be 401: %s", tc.method, tc.path, body)
		}
	})

	t.Run("founder has no workspace and is forbidden", func(t *testing.T) {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/departments", ""},
			{http.MethodPost, "/departments", `{"name":"x"}`},
			{http.MethodGet, "/teams", ""},
			{http.MethodGet, "/ai-employees", ""},
		} {
			status, body := authJSONRaw(t, tc.method, tc.path, tc.body, founder)
			require.Equal(t, http.StatusForbidden, status, "founder must be forbidden for %s %s: %s", tc.method, tc.path, body)
		}
	})

	t.Run("cross-workspace cannot read", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/departments/"+dept.ID, "", tokens.adminB)
		require.Equal(t, http.StatusNotFound, status, "cross-workspace dept read must not leak: %s", body)
		status, body = authJSONRaw(t, http.MethodGet, "/teams/"+team.ID, "", tokens.adminB)
		require.Equal(t, http.StatusNotFound, status, "cross-workspace team read must not leak: %s", body)
		status, body = authJSONRaw(t, http.MethodGet, "/ai-employees/"+emp.ID, "", tokens.adminB)
		require.Equal(t, http.StatusNotFound, status, "cross-workspace employee read must not leak: %s", body)
	})

	t.Run("cross-workspace cannot modify", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPatch, "/departments/"+dept.ID, `{"name":"stolen"}`, tokens.adminB)
		require.True(t, status == http.StatusNotFound || status == http.StatusForbidden, "got %d %s", status, body)
		status, body = authJSONRaw(t, http.MethodPatch, "/teams/"+team.ID, `{"name":"stolen"}`, tokens.adminB)
		require.True(t, status == http.StatusNotFound || status == http.StatusForbidden, "got %d %s", status, body)
		status, body = authJSONRaw(t, http.MethodPatch, "/ai-employees/"+emp.ID, `{"role":"stolen"}`, tokens.adminB)
		require.True(t, status == http.StatusNotFound || status == http.StatusForbidden, "got %d %s", status, body)

		// owner can still read original
		status, body = authJSONRaw(t, http.MethodGet, "/departments/"+dept.ID, "", tokens.adminA)
		require.Equal(t, http.StatusOK, status, body)
		var got departmentLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.NotEqual(t, "stolen", got.Name)
	})

	t.Run("guessed id is not distinguishable", func(t *testing.T) {
		fake := uuid.NewString()
		status, _ := authJSONRaw(t, http.MethodGet, "/departments/"+fake, "", tokens.adminB)
		require.Equal(t, http.StatusNotFound, status)
		status, _ = authJSONRaw(t, http.MethodGet, "/teams/"+fake, "", tokens.adminB)
		require.Equal(t, http.StatusNotFound, status)
		status, _ = authJSONRaw(t, http.MethodGet, "/ai-employees/"+fake, "", tokens.adminB)
		require.Equal(t, http.StatusNotFound, status)
	})

	t.Run("member cannot write", func(t *testing.T) {
		status, _ := authJSONRaw(t, http.MethodPost, "/departments", `{"name":"member-create"}`, tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)
		status, _ = authJSONRaw(t, http.MethodPatch, "/departments/"+dept.ID, `{"name":"member-update"}`, tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)
		status, _ = authJSONRaw(t, http.MethodDelete, "/departments/"+dept.ID, "", tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)

		status, _ = authJSONRaw(t, http.MethodPost, "/teams", fmt.Sprintf(`{"name":"x","department_id":%q}`, dept.ID), tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)
		status, _ = authJSONRaw(t, http.MethodPatch, "/teams/"+team.ID, `{"name":"member-update"}`, tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)

		status, _ = authJSONRaw(t, http.MethodPost, "/ai-employees", fmt.Sprintf(`{"name":"x","role":"y","team_id":%q}`, team.ID), tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)
		status, _ = authJSONRaw(t, http.MethodPatch, "/ai-employees/"+emp.ID, `{"role":"member-update"}`, tokens.memberA)
		require.Equal(t, http.StatusForbidden, status)
	})

	t.Cleanup(func() {
		authJSONRaw(t, http.MethodDelete, "/ai-employees/"+emp.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/departments/"+dept.ID, "", tokens.adminA)
	})
}

func TestOrganizationLiveInputValidation(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	dept := createDepartmentLive(t, tokens.adminA, "valid-dept-"+uuid.NewString()[:8])
	team := createTeamLive(t, tokens.adminA, "valid-team-"+uuid.NewString()[:8], dept.ID)
	emp := createAIEmployeeLive(t, tokens.adminA, "valid-emp-"+uuid.NewString()[:8], "role", team.ID, nil)

	cases := map[string]struct {
		method string
		path   string
		body   string
	}{
		"dept missing name":        {http.MethodPost, "/departments", `{}`},
		"dept blank name":          {http.MethodPost, "/departments", `{"name":"   "}`},
		"dept name too long":       {http.MethodPost, "/departments", fmt.Sprintf(`{"name":%q}`, strings.Repeat("a", 101))},
		"dept unknown field":       {http.MethodPost, "/departments", `{"name":"x","workspace_id":"11111111-1111-1111-1111-111111111111"}`},
		"dept malformed json":      {http.MethodPost, "/departments", `{"name":`},
		"dept malformed id":        {http.MethodGet, "/departments/not-a-uuid", ""},
		"dept unknown query":       {http.MethodGet, "/departments?workspace_id=other", ""},
		"dept non-numeric limit":   {http.MethodGet, "/departments?limit=many", ""},
		"dept negative limit":      {http.MethodGet, "/departments?limit=-1", ""},
		"dept corrupt cursor":      {http.MethodGet, "/departments?cursor=not-a-cursor", ""},
		"dept semicolon":           {http.MethodGet, "/departments?cursor=1;DROP+TABLE+departments--", ""},
		"team missing name":        {http.MethodPost, "/teams", fmt.Sprintf(`{"department_id":%q}`, dept.ID)},
		"team missing dept":        {http.MethodPost, "/teams", `{"name":"x"}`},
		"team blank name":          {http.MethodPost, "/teams", fmt.Sprintf(`{"name":"   ","department_id":%q}`, dept.ID)},
		"team name too long":       {http.MethodPost, "/teams", fmt.Sprintf(`{"name":%q,"department_id":%q}`, strings.Repeat("a", 101), dept.ID)},
		"team invalid dept id":     {http.MethodPost, "/teams", `{"name":"x","department_id":"not-a-uuid"}`},
		"team unknown field":       {http.MethodPost, "/teams", fmt.Sprintf(`{"name":"x","department_id":%q,"workspace_id":"%s"}`, dept.ID, dept.WorkspaceID)},
		"team malformed id":        {http.MethodGet, "/teams/not-a-uuid", ""},
		"team unknown query":       {http.MethodGet, "/teams?workspace_id=other", ""},
		"team invalid filter dept": {http.MethodGet, "/teams?department_id=not-a-uuid", ""},
		"team negative limit":      {http.MethodGet, "/teams?limit=-1", ""},
		"team corrupt cursor":      {http.MethodGet, "/teams?cursor=bad", ""},
		"emp missing name":         {http.MethodPost, "/ai-employees", fmt.Sprintf(`{"role":"r","team_id":%q}`, team.ID)},
		"emp missing role":         {http.MethodPost, "/ai-employees", fmt.Sprintf(`{"name":"n","team_id":%q}`, team.ID)},
		"emp missing team":         {http.MethodPost, "/ai-employees", `{"name":"n","role":"r"}`},
		"emp blank name":           {http.MethodPost, "/ai-employees", fmt.Sprintf(`{"name":"   ","role":"r","team_id":%q}`, team.ID)},
		"emp blank role":           {http.MethodPost, "/ai-employees", fmt.Sprintf(`{"name":"n","role":"   ","team_id":%q}`, team.ID)},
		"emp name too long":        {http.MethodPost, "/ai-employees", fmt.Sprintf(`{"name":%q,"role":"r","team_id":%q}`, strings.Repeat("a", 101), team.ID)},
		"emp role too long":        {http.MethodPost, "/ai-employees", fmt.Sprintf(`{"name":"n","role":%q,"team_id":%q}`, strings.Repeat("a", 101), team.ID)},
		"emp invalid team id":      {http.MethodPost, "/ai-employees", `{"name":"n","role":"r","team_id":"not-a-uuid"}`},
		"emp unknown field":        {http.MethodPost, "/ai-employees", fmt.Sprintf(`{"name":"n","role":"r","team_id":%q,"workspace_id":"x"}`, team.ID)},
		"emp malformed id":         {http.MethodGet, "/ai-employees/not-a-uuid", ""},
		"emp unknown query":        {http.MethodGet, "/ai-employees?workspace_id=other", ""},
		"emp invalid filter team":  {http.MethodGet, "/ai-employees?team_id=not-a-uuid", ""},
		"emp negative limit":       {http.MethodGet, "/ai-employees?limit=-1", ""},
		"emp corrupt cursor":       {http.MethodGet, "/ai-employees?cursor=bad", ""},
		"emp empty update":         {http.MethodPatch, "/ai-employees/" + emp.ID, `{}`},
		"team empty update":        {http.MethodPatch, "/teams/" + team.ID, `{}`},
		"dept empty body":          {http.MethodPost, "/departments", ``},
		"team empty body":          {http.MethodPost, "/teams", ``},
		"emp empty body":           {http.MethodPost, "/ai-employees", ``},
		"emp invalid task id":      {http.MethodPatch, "/ai-employees/" + emp.ID, `{"current_task_id":"not-a-uuid"}`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := authJSONRaw(t, tc.method, tc.path, tc.body, tokens.adminA)
			require.True(t, status >= 400 && status < 500, "%s must be client error got %d %s", name, status, body)
			require.NotContains(t, strings.ToLower(string(body)), "panic")
		})
	}

	t.Run("oversized limit clamped", func(t *testing.T) {
		for _, path := range []string{"/departments?limit=100000", "/teams?limit=100000", "/ai-employees?limit=100000"} {
			status, body := authJSONRaw(t, http.MethodGet, path, "", tokens.adminA)
			require.Equal(t, http.StatusOK, status, body)
			// check limit in response is capped <=200 (MaxPageSize)
			var generic struct {
				Limit int `json:"limit"`
			}
			require.NoError(t, json.Unmarshal(body, &generic))
			require.LessOrEqual(t, generic.Limit, 200, "limit must be clamped")
		}
	})

	t.Cleanup(func() {
		authJSONRaw(t, http.MethodDelete, "/ai-employees/"+emp.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/departments/"+dept.ID, "", tokens.adminA)
	})
}

func TestOrganizationLiveInvalidHierarchy(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	deptA := createDepartmentLive(t, tokens.adminA, "hier-a-"+uuid.NewString()[:8])
	deptB := createDepartmentLive(t, tokens.adminB, "hier-b-"+uuid.NewString()[:8])

	t.Run("team with non-existent department is client error", func(t *testing.T) {
		fakeDept := uuid.NewString()
		status, body := authJSONRaw(t, http.MethodPost, "/teams",
			fmt.Sprintf(`{"name":"x","department_id":%q}`, fakeDept), tokens.adminA)
		require.True(t, status >= 400 && status < 500, "must be client error got %d %s", status, body)
	})

	t.Run("team with department from other workspace is forbidden", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPost, "/teams",
			fmt.Sprintf(`{"name":"cross-dept","department_id":%q}`, deptB.ID), tokens.adminA)
		require.True(t, status == http.StatusForbidden || status == http.StatusBadRequest || status == http.StatusNotFound,
			"cross-workspace parent must be refused got %d %s", status, body)
	})

	t.Run("employee with non-existent team is client error", func(t *testing.T) {
		fakeTeam := uuid.NewString()
		status, body := authJSONRaw(t, http.MethodPost, "/ai-employees",
			fmt.Sprintf(`{"name":"x","role":"r","team_id":%q}`, fakeTeam), tokens.adminA)
		require.True(t, status >= 400 && status < 500, "got %d %s", status, body)
	})

	t.Run("employee with team from other workspace is forbidden", func(t *testing.T) {
		teamB := createTeamLive(t, tokens.adminB, "team-b-"+uuid.NewString()[:8], deptB.ID)
		status, body := authJSONRaw(t, http.MethodPost, "/ai-employees",
			fmt.Sprintf(`{"name":"cross-team","role":"r","team_id":%q}`, teamB.ID), tokens.adminA)
		require.True(t, status == http.StatusForbidden || status == http.StatusBadRequest || status == http.StatusNotFound,
			"cross-workspace team parent must be refused got %d %s", status, body)
		// cleanup
		authJSONRaw(t, http.MethodDelete, "/teams/"+teamB.ID, "", tokens.adminB)
	})

	t.Run("move team to department of other workspace is forbidden", func(t *testing.T) {
		teamA := createTeamLive(t, tokens.adminA, "move-cross-"+uuid.NewString()[:8], deptA.ID)
		status, body := authJSONRaw(t, http.MethodPatch, "/teams/"+teamA.ID,
			fmt.Sprintf(`{"department_id":%q}`, deptB.ID), tokens.adminA)
		require.True(t, status == http.StatusForbidden || status == http.StatusBadRequest || status == http.StatusNotFound,
			"cross-workspace move must be refused got %d %s", status, body)
		authJSONRaw(t, http.MethodDelete, "/teams/"+teamA.ID, "", tokens.adminA)
	})

	t.Run("move employee to team of other workspace is forbidden", func(t *testing.T) {
		teamA := createTeamLive(t, tokens.adminA, "emp-move-a-"+uuid.NewString()[:8], deptA.ID)
		empA := createAIEmployeeLive(t, tokens.adminA, "emp-move-"+uuid.NewString()[:8], "role", teamA.ID, nil)
		teamB := createTeamLive(t, tokens.adminB, "emp-move-b-"+uuid.NewString()[:8], deptB.ID)
		status, body := authJSONRaw(t, http.MethodPatch, "/ai-employees/"+empA.ID,
			fmt.Sprintf(`{"team_id":%q}`, teamB.ID), tokens.adminA)
		require.True(t, status == http.StatusForbidden || status == http.StatusBadRequest || status == http.StatusNotFound,
			"cross-workspace employee move must be refused got %d %s", status, body)
		authJSONRaw(t, http.MethodDelete, "/ai-employees/"+empA.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+teamA.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+teamB.ID, "", tokens.adminB)
	})

	t.Cleanup(func() {
		authJSONRaw(t, http.MethodDelete, "/departments/"+deptA.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/departments/"+deptB.ID, "", tokens.adminB)
	})
}

func TestOrganizationLiveDuplicate(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	base := "dup-" + uuid.NewString()[:8]
	dept := createDepartmentLive(t, tokens.adminA, "dept-"+base)

	t.Run("team duplicate within same department", func(t *testing.T) {
		team := createTeamLive(t, tokens.adminA, "team-"+base, dept.ID)
		status, body := authJSONRaw(t, http.MethodPost, "/teams",
			fmt.Sprintf(`{"name":%q,"department_id":%q}`, "team-"+base, dept.ID), tokens.adminA)
		require.Equal(t, http.StatusConflict, status, "duplicate team in same department must be 409: %s", body)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team.ID, "", tokens.adminA)
	})

	t.Run("team duplicate across departments allowed", func(t *testing.T) {
		dept2 := createDepartmentLive(t, tokens.adminA, "dept2-"+base)
		team1 := createTeamLive(t, tokens.adminA, "same-name-"+base, dept.ID)
		status, body := authJSONRaw(t, http.MethodPost, "/teams",
			fmt.Sprintf(`{"name":%q,"department_id":%q}`, "same-name-"+base, dept2.ID), tokens.adminA)
		require.Equal(t, http.StatusCreated, status, "same team name in different department should be allowed: %s", body)
		var out teamLive
		require.NoError(t, json.Unmarshal(body, &out))
		authJSONRaw(t, http.MethodDelete, "/teams/"+team1.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+out.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/departments/"+dept2.ID, "", tokens.adminA)
	})

	t.Run("employee duplicate within same team", func(t *testing.T) {
		team := createTeamLive(t, tokens.adminA, "team-emp-dup-"+base, dept.ID)
		emp := createAIEmployeeLive(t, tokens.adminA, "emp-"+base, "role", team.ID, nil)
		status, body := authJSONRaw(t, http.MethodPost, "/ai-employees",
			fmt.Sprintf(`{"name":%q,"role":"r","team_id":%q}`, "emp-"+base, team.ID), tokens.adminA)
		require.Equal(t, http.StatusConflict, status, "duplicate employee in same team must be 409: %s", body)
		authJSONRaw(t, http.MethodDelete, "/ai-employees/"+emp.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team.ID, "", tokens.adminA)
	})

	t.Run("employee duplicate across teams allowed", func(t *testing.T) {
		team1 := createTeamLive(t, tokens.adminA, "team1-"+base, dept.ID)
		team2 := createTeamLive(t, tokens.adminA, "team2-"+base, dept.ID)
		emp1 := createAIEmployeeLive(t, tokens.adminA, "same-emp-"+base, "role", team1.ID, nil)
		status, body := authJSONRaw(t, http.MethodPost, "/ai-employees",
			fmt.Sprintf(`{"name":%q,"role":"r","team_id":%q}`, "same-emp-"+base, team2.ID), tokens.adminA)
		require.Equal(t, http.StatusCreated, status, "same employee name in different team should be allowed: %s", body)
		var out aiEmployeeLive
		require.NoError(t, json.Unmarshal(body, &out))
		authJSONRaw(t, http.MethodDelete, "/ai-employees/"+emp1.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/ai-employees/"+out.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team1.ID, "", tokens.adminA)
		authJSONRaw(t, http.MethodDelete, "/teams/"+team2.ID, "", tokens.adminA)
	})

	t.Cleanup(func() {
		authJSONRaw(t, http.MethodDelete, "/departments/"+dept.ID, "", tokens.adminA)
	})
}

func TestOrganizationLiveConcurrency(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)
	results := make([]departmentLive, workers)
	errors := make([]error, workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("conc-%s-%d", uuid.NewString()[:6], idx)
			status, body := authJSONRaw(t, http.MethodPost, "/departments",
				fmt.Sprintf(`{"name":%q}`, name), tokens.adminA)
			if status != http.StatusCreated {
				errors[idx] = fmt.Errorf("expected 201 got %d %s", status, body)
				return
			}
			var out departmentLive
			if err := json.Unmarshal(body, &out); err != nil {
				errors[idx] = err
				return
			}
			results[idx] = out
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		require.NoError(t, errors[i], "concurrent create %d must succeed", i)
		require.NotEmpty(t, results[i].ID, "concurrent create %d must have id", i)
	}
	// verify all appear in listing
	status, body := authJSONRaw(t, http.MethodGet, "/departments?limit=200", "", tokens.adminA)
	require.Equal(t, http.StatusOK, status, body)
	var page departmentPageLive
	require.NoError(t, json.Unmarshal(body, &page))
	idSet := map[string]bool{}
	for _, d := range page.Departments {
		idSet[d.ID] = true
	}
	for i := 0; i < workers; i++ {
		require.True(t, idSet[results[i].ID], "concurrently created department %s must appear in listing", results[i].ID)
	}
	// cleanup
	for i := 0; i < workers; i++ {
		if results[i].ID != "" {
			authJSONRaw(t, http.MethodDelete, "/departments/"+results[i].ID, "", tokens.adminA)
		}
	}
}

func TestOrganizationLiveRLS(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	// Ensure workspace A sees only its own departments
	deptA := createDepartmentLive(t, tokens.adminA, "rls-a-"+uuid.NewString()[:8])
	deptB := createDepartmentLive(t, tokens.adminB, "rls-b-"+uuid.NewString()[:8])

	status, body := authJSONRaw(t, http.MethodGet, "/departments?limit=200", "", tokens.adminA)
	require.Equal(t, http.StatusOK, status, body)
	var pageA departmentPageLive
	require.NoError(t, json.Unmarshal(body, &pageA))
	for _, d := range pageA.Departments {
		require.Equal(t, deptA.WorkspaceID, d.WorkspaceID)
		require.NotEqual(t, deptB.ID, d.ID, "A must not see B's department")
	}

	status, body = authJSONRaw(t, http.MethodGet, "/departments?limit=200", "", tokens.adminB)
	require.Equal(t, http.StatusOK, status, body)
	var pageB departmentPageLive
	require.NoError(t, json.Unmarshal(body, &pageB))
	for _, d := range pageB.Departments {
		require.Equal(t, deptB.WorkspaceID, d.WorkspaceID)
		require.NotEqual(t, deptA.ID, d.ID)
	}

	// cleanup
	authJSONRaw(t, http.MethodDelete, "/departments/"+deptA.ID, "", tokens.adminA)
	authJSONRaw(t, http.MethodDelete, "/departments/"+deptB.ID, "", tokens.adminB)
}
