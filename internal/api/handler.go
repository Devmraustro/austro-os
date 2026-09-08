package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"austro-os/internal/auth"
	"austro-os/internal/config"
	logger "austro-os/internal/log"
)

const (
	// accessTokenTTLSeconds must match the JWT lifetime minted by
	// internal/auth (15 minutes) so clients can cache it.
	accessTokenTTLSeconds = 15 * 60
	// maxRequestBodyBytes bounds every auth request body.
	maxRequestBodyBytes = 1 << 20
)

// AuthHandler exposes the HTTP authentication endpoints. It owns the JWT
// service, the identity store and the per-route rate limiting. All credential
// errors are uniform: callers can never tell whether a username or a password
// was wrong, nor whether the identity exists.
type AuthHandler struct {
	cfg     *config.Config
	jwt     *auth.JWTService
	users   auth.UserStore
	limiter *RateLimiter
}

// NewAuthHandler builds the handler with the runtime config, the JWT service
// and the identity store (PostgreSQL in production, memory in tests).
func NewAuthHandler(cfg *config.Config, jwt *auth.JWTService, users auth.UserStore) *AuthHandler {
	if cfg == nil {
		cfg = config.Load()
	}
	return &AuthHandler{cfg: cfg, jwt: jwt, users: users, limiter: NewRateLimiter()}
}

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

type meResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	IsFounder   bool   `json:"is_founder"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// Bootstrap initializes the single founder identity from the deployment
// configuration (AUSTRO_FOUNDER_USERNAME / AUSTRO_FOUNDER_PASSWORD). It is
// enabled only when both settings are present, returns 409 once a founder
// exists (the database enforces the single-founder constraint concurrently),
// and is rate limited. There is no arbitrary self-registration: this is the
// only identity-creation operation in the system.
func (h *AuthHandler) Bootstrap(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.Allow(rateKey("bootstrap", r), bootstrapRateRule) {
		writeJSONError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	if h.cfg.FounderUsername == "" || h.cfg.FounderPassword == "" {
		writeJSONError(w, http.StatusConflict, "bootstrap not configured")
		return
	}
	hash, err := auth.HashPassword(h.cfg.FounderPassword)
	if err != nil {
		h.serverError(w, "bootstrap", err)
		return
	}
	u := &auth.UserRecord{
		Username:     h.cfg.FounderUsername,
		DisplayName:  h.cfg.FounderUsername,
		IsFounder:    true,
		PasswordHash: hash,
	}
	_, err = h.users.Create(r.Context(), u)
	if err != nil {
		if errors.Is(err, auth.ErrUserExists) {
			auditAuth(u.Username, "bootstrap", "failure", "already_initialized")
			writeJSONError(w, http.StatusConflict, "already initialized")
			return
		}
		h.serverError(w, "bootstrap", err)
		return
	}
	auditAuth(u.Username, "bootstrap", "success", "founder_created")
	writeJSON(w, http.StatusCreated, map[string]string{"status": "initialized"})
}

// Login verifies credentials and issues an access token (15 minutes) plus a
// refresh token (30 days, rotation enabled). Failures are indistinguishable:
// unknown usernames still pay one bcrypt comparison against a decoy hash.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.Allow(rateKey("login", r), loginRateRule) {
		writeJSONError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	var req credentialsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		_ = auth.CheckPasswordHash(auth.DummyPasswordHash(), req.Password)
		writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	u, err := h.users.ByUsername(r.Context(), req.Username)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			_ = auth.CheckPasswordHash(auth.DummyPasswordHash(), req.Password)
			auditAuth(req.Username, "login", "failure", "unknown_username")
			writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		h.serverError(w, "login", err)
		return
	}
	if !auth.CheckPasswordHash(u.PasswordHash, req.Password) {
		auditAuth(req.Username, "login", "failure", "wrong_password")
		writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	resp, err := h.issueTokens(r.Context(), u)
	if err != nil {
		h.serverError(w, "login", err)
		return
	}
	auditAuth(u.Username, "login", "success", "issued")
	writeJSON(w, http.StatusOK, resp)
}

// Refresh rotates a refresh token and re-issues an access token for the same
// identity. The identity is revalidated before rotation so a revoked account
// never gets a fresh session. Reused/revoked tokens are always rejected.
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.Allow(rateKey("refresh", r), refreshRateRule) {
		writeJSONError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		writeJSONError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	subject, err := h.jwt.SubjectFromRefreshToken(req.RefreshToken)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	u, err := h.users.ByID(r.Context(), subject)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeJSONError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		h.serverError(w, "refresh", err)
		return
	}
	newRefresh, err := h.jwt.Refresh(req.RefreshToken)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	access, err := h.jwt.GenerateAccessTokenWithPermissions(u.ID, u.WorkspaceID, permissionsFor(u))
	if err != nil {
		h.serverError(w, "refresh", err)
		return
	}
	auditAuth(u.Username, "refresh", "success", "rotated")
	writeJSON(w, http.StatusOK, &tokenResponse{
		AccessToken:  access,
		TokenType:    "bearer",
		ExpiresIn:    accessTokenTTLSeconds,
		RefreshToken: newRefresh,
	})
}

// Logout revokes a refresh token (session termination). Revocation is
// idempotent: revoking an already revoked or unknown token still succeeds so a
// caller can always terminate a session it holds.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.Allow(rateKey("logout", r), logoutRateRule) {
		writeJSONError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		writeJSONError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if err := h.jwt.RevokeRefreshToken(req.RefreshToken); err != nil {
		h.serverError(w, "logout", err)
		return
	}
	auditAuth("", "logout", "success", "revoked")
	w.WriteHeader(http.StatusNoContent)
}

// Me returns the authenticated caller's profile. The route is registered under
// an explicit authorization rule (GET /api/me), so reaching this handler means
// the verified claims already carry the permission.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := h.users.ByID(r.Context(), claims.ID)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.serverError(w, "me", err)
		return
	}
	writeJSON(w, http.StatusOK, &meResponse{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
		IsFounder:   u.IsFounder,
		WorkspaceID: u.WorkspaceID,
	})
}

// issueTokens mints an access token with the identity's permissions and a new
// refresh token backed by the refresh store.
func (h *AuthHandler) issueTokens(ctx context.Context, u *auth.UserRecord) (*tokenResponse, error) {
	access, err := h.jwt.GenerateAccessTokenWithPermissions(u.ID, u.WorkspaceID, permissionsFor(u))
	if err != nil {
		return nil, err
	}
	refresh, err := h.jwt.IssueRefreshToken(u.ID)
	if err != nil {
		return nil, err
	}
	return &tokenResponse{
		AccessToken:  access,
		TokenType:    "bearer",
		ExpiresIn:    accessTokenTTLSeconds,
		RefreshToken: refresh,
	}, nil
}

// permissionsFor maps an identity to the explicit authorization permissions
// carried by its access token. Founder tokens may reach their own profile;
// everything else remains deny-by-default.
func permissionsFor(u *auth.UserRecord) map[string][]string {
	if u == nil || !u.IsFounder {
		return nil
	}
	return map[string][]string{"GET": {"/api/me"}}
}

func rateKey(route string, r *http.Request) string {
	return route + ":" + clientIP(r)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (h *AuthHandler) serverError(w http.ResponseWriter, action string, err error) {
	auditAuth(action, action, "failure", "internal_error")
	logger.NewEntry("auth-handler-error").SetLevel("error").With("action", action).WithError(err).Log()
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}

func auditAuth(actorID, action, outcome, detail string) {
	logger.NewEntry("auth-audit").
		With("actor_id", actorID).
		With("action", action).
		With("outcome", outcome).
		With("detail", detail).
		Log()
}
