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

type publicationLiveResponse struct {
	ID                string `json:"id"`
	WorkspaceID       string `json:"workspace_id"`
	Title             string `json:"title"`
	Status            string `json:"status"`
	FailureReason     string `json:"failure_reason"`
	ExternalReference string `json:"external_reference"`
}

type publicationListLiveResponse struct {
	Publications []publicationLiveResponse `json:"publications"`
	Count        int                      `json:"count"`
	Limit        int                      `json:"limit"`
}

func publicationJSON(t *testing.T, method, path string, body any, token, idempotencyKey string) (int, []byte) {
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
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

func publicationAccessTokens(t *testing.T, adminA, memberA, adminB *auth.UserRecord) (string, string, string) {
	t.Helper()
	svc := rbacJWT(&config.Config{
		JWTSecret:        envOrDefault("AUSTRO_JWT_SECRET", "change-me-in-production"),
		JWTRefreshSecret: envOrDefault("AUSTRO_JWT_REFRESH_SECRET", "change-me-in-production"),
	})
	mint := func(u *auth.UserRecord) string {
		tok, err := svc.GenerateAccessTokenWithPermissions(u.ID, u.WorkspaceID, auth.RoleFor(u), rbac.PermissionsForRole(auth.RoleFor(u)))
		require.NoError(t, err)
		return tok
	}
	return mint(adminA), mint(memberA), mint(adminB)
}

func TestPublishingAPILiveJourneyAndIsolation(t *testing.T) {
	adminA, memberA, adminB := ensureRbacUsers(t)
	adminToken, memberToken, adminBToken := publicationAccessTokens(t, adminA, memberA, adminB)
	key := "live-publication-" + strings.ReplaceAll(memberA.ID, "-", "")

	body := map[string]string{"title": "Live launch", "body": "A reviewed message", "platform": "stub"}
	status, raw := publicationJSON(t, http.MethodPost, "/publications", body, memberToken, key)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created publicationLiveResponse
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, workspaceA, created.WorkspaceID)
	require.Equal(t, "queued", created.Status)

	// A browser retry with the same key returns the same aggregate and does not
	// create a second publication, even when the retry body differs.
	status, raw = publicationJSON(t, http.MethodPost, "/publications", map[string]string{
		"title": "different body is ignored for this key", "body": "different", "platform": "stub",
	}, memberToken, key)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var repeated publicationLiveResponse
	require.NoError(t, json.Unmarshal(raw, &repeated))
	require.Equal(t, created.ID, repeated.ID)

	status, raw = publicationJSON(t, http.MethodPost, "/publications/"+created.ID+"/submit", nil, memberToken, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, "review", created.Status)

	// Members may submit but cannot make the human approval decision.
	status, _ = publicationJSON(t, http.MethodPost, "/publications/"+created.ID+"/approve", nil, memberToken, "")
	require.Equal(t, http.StatusForbidden, status)

	status, raw = publicationJSON(t, http.MethodPost, "/publications/"+created.ID+"/approve", nil, adminToken, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, "approved", created.Status)

	status, raw = publicationJSON(t, http.MethodPost, "/publications/"+created.ID+"/publish", nil, adminToken, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, "published", created.Status)
	require.NotEmpty(t, created.ExternalReference, "stub delivery must persist its deterministic external reference")

	status, raw = publicationJSON(t, http.MethodGet, "/publications/"+created.ID, nil, memberToken, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &created))
	require.Equal(t, "published", created.Status)

	status, raw = publicationJSON(t, http.MethodGet, "/publications?limit=1&status=published", nil, memberToken, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var page publicationListLiveResponse
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Equal(t, 1, page.Limit)
	require.LessOrEqual(t, len(page.Publications), 1)

	// The same UUID is an ordinary not-found across workspace B; no target
	// workspace can be supplied in the request to widen this read.
	status, _ = publicationJSON(t, http.MethodGet, "/publications/"+created.ID, nil, adminBToken, "")
	require.Equal(t, http.StatusNotFound, status)

	// Unauthenticated access is denied by the authz middleware, and arbitrary
	// status PATCH semantics are not a registered operation.
	status, _ = publicationJSON(t, http.MethodGet, "/publications", nil, "", "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = publicationJSON(t, http.MethodPatch, "/publications/"+created.ID, map[string]string{"status": "published"}, adminToken, "")
	require.Equal(t, http.StatusForbidden, status)

	// Strict decoding rejects a client-supplied status field on create.
	status, _ = publicationJSON(t, http.MethodPost, "/publications", map[string]any{
		"title": "bad", "body": "bad", "platform": "stub", "status": "published",
	}, memberToken, "strict-publication-body")
	require.Equal(t, http.StatusBadRequest, status)
}

func TestPublishingUIIsARealLifecycleSurface(t *testing.T) {
	root := repoRoot(t)
	index, err := os.ReadFile(filepath.Join(root, "internal", "webui", "static", "index.html"))
	require.NoError(t, err)
	app, err := os.ReadFile(filepath.Join(root, "internal", "webui", "static", "app.js"))
	require.NoError(t, err)
	page := string(index)
	js := string(app)
	require.Contains(t, page, `id="publications-card"`)
	for _, route := range []string{"/publications", "/submit", "/approve", "/reject", "/publish", "/retry"} {
		require.Contains(t, js, route)
	}
	require.Contains(t, js, `headers["Idempotency-Key"]`)
	require.NotContains(t, page, "Publishing approval queue")
}
EOF

git diff --check; git status --short | tail -20```
,