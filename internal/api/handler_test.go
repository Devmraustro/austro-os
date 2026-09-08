package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/config"

	"github.com/stretchr/testify/require"
)

func testHandler(founder bool) *AuthHandler {
	cfg := &config.Config{
		JWTSecret:        "test-access-secret-at-least-32-chars-long!!",
		JWTRefreshSecret: "test-refresh-secret-at-least-32-chars-long!!",
	}
	if founder {
		cfg.FounderUsername = "founder"
		cfg.FounderPassword = "test-founder-password"
	}
	jwt := auth.InitializeWithStore(cfg, auth.NewMemoryRefreshStore())
	return NewAuthHandler(cfg, jwt, auth.NewMemoryUserStore())
}

func doRequest(t *testing.T, h *AuthHandler, method, path, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Buffer
	if body == "" {
		rdr = bytes.NewBuffer(nil)
	} else {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/bootstrap", h.Bootstrap)
	mux.HandleFunc("POST /api/auth/login", h.Login)
	mux.HandleFunc("POST /api/auth/refresh", h.Refresh)
	mux.HandleFunc("POST /api/auth/logout", h.Logout)
	mux.HandleFunc("GET /api/me", h.Me)
	handler := h.RequireAuth(mux)
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

func TestBootstrapNotConfiguredConflict(t *testing.T) {
	h := testHandler(false)
	rec := doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestBootstrapInitializesThenConflicts(t *testing.T) {
	h := testHandler(true)
	first := doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	require.Equal(t, http.StatusCreated, first.Code)

	second := doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	require.Equal(t, http.StatusConflict, second.Code)
}

func builtinMe(t *testing.T, h *AuthHandler, token string, expectStatus int) meResponse {
	t.Helper()
	rec := doRequest(t, h, http.MethodGet, "/api/me", "", token)
	require.Equal(t, expectStatus, rec.Code)
	return decodeBody[meResponse](t, rec)
}

func login(t *testing.T, h *AuthHandler, username, password string) tokenResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	rec := doRequest(t, h, http.MethodPost, "/api/auth/login", string(body), "")
	require.Equal(t, http.StatusOK, rec.Code)
	return decodeBody[tokenResponse](t, rec)
}

func TestLoginFlowAndProfile(t *testing.T) {
	h := testHandler(true)
	boot := doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	require.Equal(t, http.StatusCreated, boot.Code)

	got := login(t, h, "founder", "test-founder-password")
	require.NotEmpty(t, got.AccessToken)
	require.NotEmpty(t, got.RefreshToken)
	require.Equal(t, "bearer", got.TokenType)
	require.Equal(t, accessTokenTTLSeconds, got.ExpiresIn)

	me := builtinMe(t, h, got.AccessToken, http.StatusOK)
	require.Equal(t, "founder", me.Username)
	require.True(t, me.IsFounder)
	require.Equal(t, "", me.WorkspaceID)
}

func TestLoginUnknownUsernameUniformUnauthorized(t *testing.T) {
	h := testHandler(true)
	body := `{"username":"nobody","password":"test-founder-password"}`
	rec := doRequest(t, h, http.MethodPost, "/api/auth/login", body, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid credentials")
}

func TestLoginWrongPasswordUnauthorized(t *testing.T) {
	h := testHandler(true)
	doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	body := `{"username":"founder","password":"not-the-password"}`
	rec := doRequest(t, h, http.MethodPost, "/api/auth/login", body, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid credentials")
}

func TestLoginEmptyPayloadUnauthorized(t *testing.T) {
	h := testHandler(true)
	rec := doRequest(t, h, http.MethodPost, "/api/auth/login", `{}`, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMeRequiresAuthentication(t *testing.T) {
	h := testHandler(true)
	rec := doRequest(t, h, http.MethodGet, "/api/me", "", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	bad := doRequest(t, h, http.MethodGet, "/api/me", "", "not-a-real-token")
	require.Equal(t, http.StatusUnauthorized, bad.Code)
}

func TestRefreshRotationAndReuseRejection(t *testing.T) {
	h := testHandler(true)
	doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	got := login(t, h, "founder", "test-founder-password")

	body := `{"refresh_token":"` + got.RefreshToken + `"}`
	first := doRequest(t, h, http.MethodPost, "/api/auth/refresh", body, "")
	require.Equal(t, http.StatusOK, first.Code)
	rotated := decodeBody[tokenResponse](t, first)
	require.NotEqual(t, got.RefreshToken, rotated.RefreshToken)
	require.NotEmpty(t, rotated.AccessToken)

	// The rotation invalidated the old token family: reuse must be rejected.
	reuse := doRequest(t, h, http.MethodPost, "/api/auth/refresh", body, "")
	require.Equal(t, http.StatusUnauthorized, reuse.Code)
}

func TestRefreshRejectsGarbage(t *testing.T) {
	h := testHandler(true)
	body := `{"refresh_token":"garbage"}`
	rec := doRequest(t, h, http.MethodPost, "/api/auth/refresh", body, "")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestLogoutRevokesRefreshToken(t *testing.T) {
	h := testHandler(true)
	doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	got := login(t, h, "founder", "test-founder-password")

	out := doRequest(t, h, http.MethodPost, "/api/auth/logout", `{"refresh_token":"`+got.RefreshToken+`"}`, "")
	require.Equal(t, http.StatusNoContent, out.Code)

	// The revoked refresh token can no longer rotate into a new session.
	refresh := doRequest(t, h, http.MethodPost, "/api/auth/refresh", `{"refresh_token":"`+got.RefreshToken+`"}`, "")
	require.Equal(t, http.StatusUnauthorized, refresh.Code)

	// The already-issued access token keeps working until it expires.
	me := builtinMe(t, h, got.AccessToken, http.StatusOK)
	require.Equal(t, "founder", me.Username)
}

func TestLoginRateLimit(t *testing.T) {
	h := testHandler(true)
	var last *httptest.ResponseRecorder
	for i := 0; i <= loginRateRule.max; i++ {
		last = doRequest(t, h, http.MethodPost, "/api/auth/login", `{"username":"founder","password":"x"}`, "")
	}
	require.Equal(t, http.StatusTooManyRequests, last.Code)
}

func TestRequireAuthRejectsInvalidBearer(t *testing.T) {
	h := testHandler(true)
	rec := doRequest(t, h, http.MethodGet, "/api/me", "", "totally-invalid")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestPermissionsGateProfile(t *testing.T) {
	// The deny-by-default authorization contract for /api/me: an explicit rule
	// exists, so only tokens carrying the GET /api/me permission pass.
	h := testHandler(true)
	doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")

	az := authz.NewAuthorizer()
	az.AddRule("GET", "/api/me")

	foundToken := login(t, h, "founder", "test-founder-password")
	allowedClaims, err := h.jwt.VerifyAccessToken(foundToken.AccessToken)
	require.NoError(t, err)
	require.NoError(t, az.AuthorizeClaims(allowedClaims, "GET", "/api/me"))

	noPerms := &auth.Claims{ID: "someone", Permissions: nil}
	require.Error(t, az.AuthorizeClaims(noPerms, "GET", "/api/me"))

	// A route with no rule is denied even for a founder (no permissive fallback).
	require.Error(t, az.AuthorizeClaims(allowedClaims, "GET", "/api/workspaces"))
}
