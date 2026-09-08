package austro_os_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// workspaceLive mirrors the API contract for the workspace administration
// endpoints.
type workspaceLive struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// loginLive authenticates against the live API and returns the token pair.
func loginLive(t *testing.T, username, password string) authTokenResponse {
	t.Helper()
	status, body := authJSON(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": username, "password": password}, "")
	require.Equal(t, http.StatusOK, status, "login must succeed: %s", body)
	var login authTokenResponse
	require.NoError(t, json.Unmarshal(body, &login))
	require.NotEmpty(t, login.AccessToken)
	return login
}

// authJSONRaw issues a JSON request with an exact raw body (for malformed and
// unknown-field payloads) and returns status + raw body.
func authJSONRaw(t *testing.T, method, path, raw string, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, getEnv().apiURL+path, bytes.NewBufferString(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

// TestWorkspaceAdminLiveEndToEnd exercises the live API's three workspace
// administration endpoints against the running stack: founder organization-level
// list/create/read, workspace-admin own-workspace read only, member denial,
// strict input validation, deterministic conflict, and no existence leak.
func TestWorkspaceAdminLiveEndToEnd(t *testing.T) {
	adminA, memberA, _ := ensureRbacUsers(t)
	founderUsername := envOrDefault("AUSTRO_FOUNDER_USERNAME", "founder")
	founderPassword := envOrDefault("AUSTRO_FOUNDER_PASSWORD", "founder-dev-password-min-16-chars")

	founder := loginLive(t, founderUsername, founderPassword)
	admin := loginLive(t, adminA.Username, rbacPassword)
	member := loginLive(t, memberA.Username, rbacPassword)

	db, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer db.Close()

	// 1. Unauthenticated and invalid-token denials.
	status, _ := authJSON(t, http.MethodGet, "/workspaces", nil, "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = authJSON(t, http.MethodGet, "/workspaces", nil, "garbage")
	require.Equal(t, http.StatusUnauthorized, status)

	// 2. Founder organization-level listing must include the seeded workspaces.
	status, body := authJSON(t, http.MethodGet, "/workspaces", nil, founder.AccessToken)
	require.Equal(t, http.StatusOK, status)
	var ws []workspaceLive
	require.NoError(t, json.Unmarshal(body, &ws))
	byName := map[string]string{}
	for _, w := range ws {
		byName[w.Name] = w.ID
	}
	require.Equal(t, workspaceA, byName["Workspace A"])
	require.Equal(t, workspaceB, byName["Workspace B"])

	// 3. Founder reads any workspace by id.
	status, body = authJSON(t, http.MethodGet, "/workspaces/"+workspaceA, nil, founder.AccessToken)
	require.Equal(t, http.StatusOK, status)
	var got workspaceLive
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, workspaceA, got.ID)
	require.Equal(t, "Workspace A", got.Name)

	// 4. Founder creates a workspace, reads it back, then hits the
	//    deterministic conflict and strict validation errors.
	newName := "Live Admin Workspace " + uuid.NewString()[:8]
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workspaces WHERE name = $1`, newName)
	})
	status, body = authJSON(t, http.MethodPost, "/workspaces",
		map[string]string{"name": newName}, founder.AccessToken)
	require.Equal(t, http.StatusCreated, status)
	var created workspaceLive
	require.NoError(t, json.Unmarshal(body, &created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, newName, created.Name)
	require.NotEmpty(t, created.CreatedAt)
	require.NotEmpty(t, created.UpdatedAt)

	status, _ = authJSON(t, http.MethodGet, "/workspaces/"+created.ID, nil, founder.AccessToken)
	require.Equal(t, http.StatusOK, status)

	status, _ = authJSON(t, http.MethodPost, "/workspaces",
		map[string]string{"name": newName}, founder.AccessToken)
	require.Equal(t, http.StatusConflict, status, "duplicate workspace name must be deterministic")

	status, _ = authJSON(t, http.MethodPost, "/workspaces", map[string]string{"name": ""}, founder.AccessToken)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = authJSON(t, http.MethodPost, "/workspaces", map[string]string{"name": "   "}, founder.AccessToken)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = authJSONRaw(t, http.MethodPost, "/workspaces",
		`{"name":"ok","workspace_id":"`+workspaceB+`"}`, founder.AccessToken)
	require.Equal(t, http.StatusBadRequest, status, "client-supplied workspace_id must be rejected as an unknown field")
	status, _ = authJSONRaw(t, http.MethodPost, "/workspaces", `{not json`, founder.AccessToken)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = authJSON(t, http.MethodPost, "/workspaces",
		map[string]string{"name": strings.Repeat("A", 101)}, founder.AccessToken)
	require.Equal(t, http.StatusBadRequest, status)

	// 5. Workspace admin: own workspace read only.
	status, body = authJSON(t, http.MethodGet, "/workspaces/"+workspaceA, nil, admin.AccessToken)
	require.Equal(t, http.StatusOK, status)
	var own workspaceLive
	require.NoError(t, json.Unmarshal(body, &own))
	require.Equal(t, workspaceA, own.ID)

	status, _ = authJSON(t, http.MethodGet, "/workspaces/"+workspaceB, nil, admin.AccessToken)
	require.Equal(t, http.StatusForbidden, status, "admin must be denied another workspace")
	status, _ = authJSON(t, http.MethodGet, "/workspaces", nil, admin.AccessToken)
	require.Equal(t, http.StatusForbidden, status, "admin must never enumerate workspaces")
	status, _ = authJSON(t, http.MethodPost, "/workspaces",
		map[string]string{"name": "admin-forbidden"}, admin.AccessToken)
	require.Equal(t, http.StatusForbidden, status, "admin must not create workspaces")

	// 6. Workspace member: the whole surface is denied.
	status, _ = authJSON(t, http.MethodGet, "/workspaces/"+workspaceA, nil, member.AccessToken)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = authJSON(t, http.MethodGet, "/workspaces", nil, member.AccessToken)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = authJSON(t, http.MethodPost, "/workspaces",
		map[string]string{"name": "member-forbidden"}, member.AccessToken)
	require.Equal(t, http.StatusForbidden, status)

	// 7. Founder not-found and invalid-id semantics (no existence detail leak
	//    in the message itself).
	status, body = authJSON(t, http.MethodGet, "/workspaces/"+uuid.NewString(), nil, founder.AccessToken)
	require.Equal(t, http.StatusNotFound, status)
	require.Contains(t, string(body), "workspace not found")
	status, _ = authJSON(t, http.MethodGet, "/workspaces/not-a-uuid", nil, founder.AccessToken)
	require.Equal(t, http.StatusBadRequest, status)
}

// TestWorkspacesRLSOrgLevelScoped proves the workspaces table is still
// strongly scoped by RLS: a restricted workspace principal sees exactly its own
// workspace row when bound, and nothing when no workspace context is set — the
// organization-level listing is founder-only at the authorization layer and
// never reachable through the SQL layer by a workspace-bound principal.
func TestWorkspacesRLSOrgLevelScoped(t *testing.T) {
	db, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, ensureIsolationRoles(db, "Workspace A", "Workspace B"))

	seen := namesOf(t, workspaceA)
	require.Equal(t, []string{"Workspace A"}, seen, "a principal bound to A sees only A's workspace row")

	seenB := namesOf(t, workspaceB)
	require.Equal(t, []string{"Workspace B"}, seenB, "a principal bound to B sees only B's workspace row")

	// Unbound restricted connection: the RLS policy exposes nothing (the
	// founder-only org listing cannot be reached through the SQL layer).
	unbound, err := connectAs(getEnv().postgresDSN, workspaceARole)
	require.NoError(t, err)
	defer unbound.Close()
	rows, err := unbound.Query(`SELECT name FROM workspaces`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	require.Empty(t, names, "an unbound restricted role must never list workspaces")
}

// namesOf returns the workspace names a restricted role sees when bound to ws.
func namesOf(t *testing.T, ws string) []string {
	t.Helper()
	role := workspaceARole
	if ws == workspaceB {
		role = workspaceBRole
	}
	rDB, err := connectAs(getEnv().postgresDSN, role)
	require.NoError(t, err)
	defer rDB.Close()
	_, err = rDB.Exec(`SELECT set_config('app.current_workspace', $1, false)`, ws)
	require.NoError(t, err)
	rows, err := rDB.Query(`SELECT name FROM workspaces`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}
