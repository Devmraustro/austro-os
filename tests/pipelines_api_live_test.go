package austro_os_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"austro-os/internal/auth"
	"austro-os/internal/config"
	"austro-os/internal/rbac"
	"github.com/stretchr/testify/require"
)

type pipelineLiveResponse struct {
	ID                string  `json:"id"`
	WorkspaceID       string  `json:"workspace_id"`
	Stage             string  `json:"stage"`
	Status            string  `json:"status"`
	ResearchReference string  `json:"research_reference"`
	ScriptReference   string  `json:"script_reference"`
	ReviewReference   string  `json:"review_reference"`
	PublicationID     *string `json:"publication_id"`
	FailureReason     string  `json:"failure_reason"`
	RetryCount        int     `json:"retry_count"`
	ApprovedBy        string  `json:"approved_by"`
}

type pipelinePageLiveResponse struct {
	Pipelines []pipelineLiveResponse `json:"pipelines"`
	Count     int                    `json:"count"`
	Limit     int                    `json:"limit"`
}

func pipelineLiveJSON(t *testing.T, method, path string, body any, token, key string) (int, []byte) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, getEnv().apiURL+path, payload)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

func pipelineLiveTokens(t *testing.T, adminA, memberA, adminB *auth.UserRecord) (string, string, string) {
	t.Helper()
	svc := rbacJWT(&config.Config{
		JWTSecret:        envOrDefault("AUSTRO_JWT_SECRET", "change-me-in-production"),
		JWTRefreshSecret: envOrDefault("AUSTRO_JWT_REFRESH_SECRET", "change-me-in-production"),
	})
	mint := func(u *auth.UserRecord) string {
		token, err := svc.GenerateAccessTokenWithPermissions(u.ID, u.WorkspaceID, auth.RoleFor(u), rbac.PermissionsForRole(auth.RoleFor(u)))
		require.NoError(t, err)
		return token
	}
	return mint(adminA), mint(memberA), mint(adminB)
}

func TestCreatorPipelineAPILiveJourneyIsolationAndApproval(t *testing.T) {
	adminA, memberA, adminB := ensureRbacUsers(t)
	adminToken, memberToken, adminBToken := pipelineLiveTokens(t, adminA, memberA, adminB)
	key := "live-pipeline-" + strings.ReplaceAll(memberA.ID, "-", "")

	status, raw := pipelineLiveJSON(t, http.MethodPost, "/pipelines", map[string]any{}, memberToken, key)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created pipelineLiveResponse
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, workspaceA, created.WorkspaceID)
	require.Equal(t, "research", created.Stage)
	require.Equal(t, "created", created.Status)

	// A retry with the same key is idempotent and cannot create a second
	// pipeline even when the second body contains an unsupported field.
	status, raw = pipelineLiveJSON(t, http.MethodPost, "/pipelines", map[string]any{"unexpected": true}, memberToken, key)
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	status, raw = pipelineLiveJSON(t, http.MethodPost, "/pipelines", map[string]any{}, memberToken, key)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var repeated pipelineLiveResponse
	require.NoError(t, json.Unmarshal(raw, &repeated))
	require.Equal(t, created.ID, repeated.ID)

	status, raw = pipelineLiveJSON(t, http.MethodGet, "/pipelines?limit=1", nil, memberToken, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var page pipelinePageLiveResponse
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Equal(t, 1, page.Limit)
	require.LessOrEqual(t, len(page.Pipelines), 1)

	// The worker performs research and script, then persists real references and
	// stops at the review/approval handoff. There is no client-controlled stage.
	require.Eventually(t, func() bool {
		status, raw := pipelineLiveJSON(t, http.MethodGet, "/pipelines/"+created.ID, nil, memberToken, "")
		if status != http.StatusOK {
			return false
		}
		var current pipelineLiveResponse
		if json.Unmarshal(raw, &current) != nil {
			return false
		}
		created = current
		return current.Stage == "review" && current.Status == "awaiting_approval"
	}, 90*time.Second, time.Second, "pipeline must reach the persisted human approval handoff")
	require.NotEmpty(t, created.ResearchReference)
	require.NotEmpty(t, created.ScriptReference)
	require.NotEmpty(t, created.ReviewReference)
	require.NotNil(t, created.PublicationID)

	// Members can observe but cannot approve. The admin command is the only
	// operation that opens publish, and it carries the verified actor identity.
	status, _ = pipelineLiveJSON(t, http.MethodPost, "/pipelines/"+created.ID+"/approve", nil, memberToken, "")
	require.Equal(t, http.StatusForbidden, status)
	status, raw = pipelineLiveJSON(t, http.MethodPost, "/pipelines/"+created.ID+"/approve", nil, adminToken, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, "approved", created.Status)
	require.Equal(t, "review", created.Stage)
	require.NotEmpty(t, created.ApprovedBy)

	// The approval event resumes the existing worker; the API does not fake
	// progress or expose a publish/complete mutation.
	require.Eventually(t, func() bool {
		status, raw := pipelineLiveJSON(t, http.MethodGet, "/pipelines/"+created.ID, nil, memberToken, "")
		if status != http.StatusOK {
			return false
		}
		var current pipelineLiveResponse
		if json.Unmarshal(raw, &current) != nil {
			return false
		}
		created = current
		return current.Stage == "complete" && current.Status == "done"
	}, 90*time.Second, time.Second, "approved pipeline must complete through the worker")

	// IDOR and malformed query behavior are explicit: the foreign workspace
	// sees a not-found, arbitrary mutation is not routed, and unknown query
	// parameters do not silently widen a list.
	status, _ = pipelineLiveJSON(t, http.MethodGet, "/pipelines/"+created.ID, nil, adminBToken, "")
	require.Equal(t, http.StatusNotFound, status)
	status, _ = pipelineLiveJSON(t, http.MethodGet, "/pipelines?limit=1&unknown=x", nil, memberToken, "")
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = pipelineLiveJSON(t, http.MethodPatch, "/pipelines/"+created.ID, map[string]string{"stage": "complete"}, adminToken, "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = pipelineLiveJSON(t, http.MethodGet, "/pipelines/"+created.ID, nil, "", "")
	require.Equal(t, http.StatusForbidden, status)

	adminDB, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminDB.Exec(`DELETE FROM pipelines WHERE id=$1`, created.ID)
		if created.PublicationID != nil {
			_, _ = adminDB.Exec(`DELETE FROM publications WHERE id=$1`, *created.PublicationID)
		}
		_ = adminDB.Close()
	})
}

func TestCreatorUIIsARealPipelineSurface(t *testing.T) {
	root := repoRoot(t)
	index, err := os.ReadFile(filepath.Join(root, "internal", "webui", "static", "index.html"))
	require.NoError(t, err)
	app, err := os.ReadFile(filepath.Join(root, "internal", "webui", "static", "app.js"))
	require.NoError(t, err)
	require.Contains(t, string(index), `id="pipelines-card"`)
	for _, route := range []string{"/pipelines", "/approve", "/retry"} {
		require.Contains(t, string(app), route)
	}
	require.Contains(t, string(index), "worker progress will appear from the server")
	require.NotContains(t, string(index), "Set stage")
}
