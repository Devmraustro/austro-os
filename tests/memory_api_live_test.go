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

type memoryLiveResponse struct {
	Layer       string `json:"layer"`
	Key         string `json:"key"`
	Value       string `json:"value"`
	WorkspaceID string `json:"workspace_id"`
	TTLSeconds  *int64 `json:"ttl_seconds"`
}

func memoryJSON(t *testing.T, method, path string, body any, token string) (int, []byte) {
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
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

func memoryAccessTokens(t *testing.T, adminA, adminB *auth.UserRecord) (string, string) {
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
	return mint(adminA), mint(adminB)
}

func TestMemoryAPILiveWorkspaceScopedReadWrite(t *testing.T) {
	adminA, _, adminB := ensureRbacUsers(t)
	tokenA, tokenB := memoryAccessTokens(t, adminA, adminB)
	key := "live-memory-" + strings.ReplaceAll(adminA.ID, "-", "")

	status, body := memoryJSON(t, http.MethodPut, "/memory/workspace/"+key,
		map[string]any{"value": "workspace A value", "ttl_seconds": 60}, tokenA)
	require.Equal(t, http.StatusOK, status, string(body))
	var written memoryLiveResponse
	require.NoError(t, json.Unmarshal(body, &written))
	require.Equal(t, "workspace", written.Layer)
	require.Equal(t, key, written.Key)
	require.Equal(t, "workspace A value", written.Value)
	require.NotNil(t, written.TTLSeconds)
	require.Equal(t, int64(60), *written.TTLSeconds)
	require.Equal(t, workspaceA, written.WorkspaceID,
		"workspace must be derived from the verified identity")

	status, body = memoryJSON(t, http.MethodGet, "/memory/workspace/"+key, nil, tokenA)
	require.Equal(t, http.StatusOK, status, string(body))
	var read memoryLiveResponse
	require.NoError(t, json.Unmarshal(body, &read))
	require.Equal(t, written.Layer, read.Layer)
	require.Equal(t, written.Key, read.Key)
	require.Equal(t, written.Value, read.Value)
	require.Equal(t, written.WorkspaceID, read.WorkspaceID)
	require.Nil(t, read.TTLSeconds, "GET does not claim a TTL it cannot retrieve")

	// The same URL/key in workspace B is a normal miss, not a cross-tenant hit.
	status, _ = memoryJSON(t, http.MethodGet, "/memory/workspace/"+key, nil, tokenB)
	require.Equal(t, http.StatusNotFound, status)
}

func TestMemoryAPILiveStrictBoundaryAndAuthorization(t *testing.T) {
	adminA, _, _ := ensureRbacUsers(t)
	token, _ := memoryAccessTokens(t, adminA, adminA)

	for _, layer := range []string{"org", "organizational", "SESSION", "unknown"} {
		status, _ := memoryJSON(t, http.MethodGet, "/memory/"+layer+"/strict-key", nil, token)
		require.Equal(t, http.StatusBadRequest, status, "layer %q must not be accepted", layer)
	}

	status, _ := memoryJSON(t, http.MethodGet, "/memory/workspace/strict-key?workspace_id="+workspaceB, nil, token)
	require.Equal(t, http.StatusBadRequest, status, "client workspace query must be rejected")

	status, _ = memoryJSON(t, http.MethodPut, "/memory/workspace/strict-key", map[string]any{
		"value": "safe", "workspace_id": workspaceB,
	}, token)
	require.Equal(t, http.StatusBadRequest, status, "client workspace body must be rejected")

	status, _ = memoryJSON(t, http.MethodPut, "/memory/workspace/strict-key",
		map[string]any{"value": "safe"}, token)
	require.Equal(t, http.StatusOK, status)

	// A second JSON value is not silently ignored.
	req, err := http.NewRequest(http.MethodPut, getEnv().apiURL+"/memory/workspace/trailing-key",
		strings.NewReader(`{"value":"first"}{"value":"second"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	status, _ = memoryJSON(t, http.MethodGet, "/memory/workspace/strict-key", nil, "")
	require.Equal(t, http.StatusForbidden, status, "missing auth is denied by the route authorizer")
	status, _ = memoryJSON(t, http.MethodGet, "/memory/workspace/strict-key", nil, "not-a-token")
	require.Equal(t, http.StatusUnauthorized, status, "malformed bearer token is rejected")
}

func TestMemoryAPILiveDropsMalformedCorrelationIDs(t *testing.T) {
	adminA, _, _ := ensureRbacUsers(t)
	token, _ := memoryAccessTokens(t, adminA, adminA)
	key := "live-memory-trace-" + strings.ReplaceAll(adminA.ID, "-", "")

	// Correlation ids come from request headers, so they are client-supplied.
	// A malformed one must not turn an otherwise valid memory write into a 500:
	// the persistent sink drops it, the same convention the publishing and
	// pipeline audit adapters already use.
	payload := strings.NewReader(`{"value":"trace-safe","ttl_seconds":60}`)
	req, err := http.NewRequest(http.MethodPut, getEnv().apiURL+"/memory/workspace/"+key, payload)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Trace-ID", "not-a-uuid")
	req.Header.Set("X-Span-ID", "../etc/passwd")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	status, body := memoryJSON(t, http.MethodGet, "/memory/workspace/"+key, nil, token)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), "trace-safe")
}

func TestMemoryUIExposesOnlyReadWriteOperations(t *testing.T) {
	root := repoRoot(t)
	index, err := os.ReadFile(filepath.Join(root, "internal", "webui", "static", "index.html"))
	require.NoError(t, err)
	app, err := os.ReadFile(filepath.Join(root, "internal", "webui", "static", "app.js"))
	require.NoError(t, err)

	require.Contains(t, string(index), `id="memory-card"`)
	require.Contains(t, string(app), `authenticated("GET", memoryPath())`)
	require.Contains(t, string(app), `authenticated("PUT", memoryPath(), payload)`)
	memorySection := string(app)[strings.Index(string(app), "/* ---------- memory ---------- */"):]
	require.NotContains(t, memorySection, `authenticated("DELETE"`)
	require.NotContains(t, memorySection, `authenticated("POST"`)
	require.NotContains(t, memorySection, "search")
}
