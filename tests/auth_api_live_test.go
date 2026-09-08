package austro_os_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// tokenResponse mirrors the API contract for POST /api/auth/*
type authTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

type authMeResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	IsFounder   bool   `json:"is_founder"`
	WorkspaceID string `json:"workspace_id"`
}

// authJSON issues a JSON request to the live API and returns the status and
// raw body. The API container and this test read the same AUSTRO_FOUNDER_*
// environment (both inherit the compose project), so the founder credentials
// used here are the ones bootstrapped by the API.
func authJSON(t *testing.T, method, path string, body map[string]string, token string) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequest(method, getEnv().apiURL+path, &buf)
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

// TestAuthLiveEndToEnd exercises the real running API: bootstrap (idempotent),
// login, profile, refresh rotation, logout, and the deny-by-default behavior
// for missing/bad credentials. The first run on a fresh database creates the
// founder; subsequent runs see 409 from bootstrap and still log in.
func TestAuthLiveEndToEnd(t *testing.T) {
	founderUsername := envOrDefault("AUSTRO_FOUNDER_USERNAME", "founder")
	founderPassword := envOrDefault("AUSTRO_FOUNDER_PASSWORD", "founder-dev-password-min-16-chars")

	// Bootstrap: 201 on first initialization, 409 when already initialized.
	status, body := authJSON(t, http.MethodPost, "/api/auth/bootstrap", map[string]string{}, "")
	require.Contains(t, []int{http.StatusCreated, http.StatusConflict}, status,
		"bootstrap must initialize once (201) or report already initialized (409), got %d: %s", status, body)

	// Wrong credentials never reveal whether the identity exists.
	status, _ = authJSON(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": founderUsername, "password": "definitely-wrong"}, "")
	require.Equal(t, http.StatusUnauthorized, status)

	// Login with the configured founder credentials.
	status, body = authJSON(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": founderUsername, "password": founderPassword}, "")
	require.Equal(t, http.StatusOK, status)
	var login authTokenResponse
	require.NoError(t, json.Unmarshal(body, &login))
	require.NotEmpty(t, login.AccessToken)
	require.NotEmpty(t, login.RefreshToken)
	require.Equal(t, "bearer", login.TokenType)
	require.Equal(t, 15*60, login.ExpiresIn)

	// /api/me requires a valid token; a profile comes back for the founder.
	status, _ = authJSON(t, http.MethodGet, "/api/me", nil, "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = authJSON(t, http.MethodGet, "/api/me", nil, "garbage-token")
	require.Equal(t, http.StatusUnauthorized, status)

	status, body = authJSON(t, http.MethodGet, "/api/me", nil, login.AccessToken)
	require.Equal(t, http.StatusOK, status)
	var me authMeResponse
	require.NoError(t, json.Unmarshal(body, &me))
	require.Equal(t, founderUsername, me.Username)
	require.True(t, me.IsFounder)
	require.Equal(t, "founder", me.Role, "/api/me must report the founder role")
	require.Equal(t, "", me.WorkspaceID, "the founder is organization-level, not workspace-scoped")

	// Refresh rotation: old refresh token is invalidated, new one issued.
	status, body = authJSON(t, http.MethodPost, "/api/auth/refresh",
		map[string]string{"refresh_token": login.RefreshToken}, "")
	require.Equal(t, http.StatusOK, status)
	var rotated authTokenResponse
	require.NoError(t, json.Unmarshal(body, &rotated))
	require.NotEmpty(t, rotated.AccessToken)
	require.NotEqual(t, login.RefreshToken, rotated.RefreshToken)

	// Reusing the consumed refresh token must be rejected.
	status, _ = authJSON(t, http.MethodPost, "/api/auth/refresh",
		map[string]string{"refresh_token": login.RefreshToken}, "")
	require.Equal(t, http.StatusUnauthorized, status)

	// Logout revokes the latest refresh token.
	status, _ = authJSON(t, http.MethodPost, "/api/auth/logout",
		map[string]string{"refresh_token": rotated.RefreshToken}, "")
	require.Equal(t, http.StatusNoContent, status)
	status, _ = authJSON(t, http.MethodPost, "/api/auth/refresh",
		map[string]string{"refresh_token": rotated.RefreshToken}, "")
	require.Equal(t, http.StatusUnauthorized, status)

	// The already-issued access token keeps working within its 15-minute window.
	status, body = authJSON(t, http.MethodGet, "/api/me", nil, rotated.AccessToken)
	require.Equal(t, http.StatusOK, status)
}
