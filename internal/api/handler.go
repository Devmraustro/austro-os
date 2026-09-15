package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/config"
	logger "austro-os/internal/log"
	"austro-os/internal/middleware"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
)

// errAuditUnavailable is returned by recordAudit when no durable sink is wired.
// A security-relevant operation must not be able to report success without a
// persisted record, so this is surfaced to the caller rather than logged.
var errAuditUnavailable = errors.New("durable audit unavailable")

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
	// auditSink persists authentication events to the append-only audit
	// chain. It is nil only in unit tests; a production handler always has
	// one, and every security-relevant outcome below fails closed when it is
	// missing.
	auditSink audit.Sink
}

// SetAuditSink attaches the persistent audit writer. It is a setter rather than
// a constructor argument so that the many existing construction sites (mostly
// tests with no database) stay valid, while main.go wires the real store.
func (h *AuthHandler) SetAuditSink(s audit.Sink) *AuthHandler {
	h.auditSink = s
	return h
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
	Role        string `json:"role"`
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
		Role:         rbac.RoleFounder,
		PasswordHash: hash,
	}
	_, err = h.users.Create(r.Context(), u)
	if err != nil {
		if errors.Is(err, auth.ErrUserExists) {
			auditAuth(u.Username, "bootstrap", "failure", "already_initialized")
			_ = h.recordAudit(r, authEvent("", "auth.bootstrap", "denied", "already_initialized"))
			writeJSONError(w, http.StatusConflict, "already initialized")
			return
		}
		h.serverError(w, "bootstrap", err)
		return
	}
	auditAuth(u.Username, "bootstrap", "success", "founder_created")
	// The founder identity is the root of the whole authorization model.
	// Creating it without a durable audit record must not be reported as a
	// success, so a persistence failure becomes a 500 and the caller retries.
	if err := h.recordAudit(r, authEvent(u.ID, "auth.bootstrap", "success", "founder_created")); err != nil {
		h.serverError(w, "bootstrap", err)
		return
	}
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
			_ = h.recordAudit(r, authEvent("", "auth.login", "denied", "invalid_credentials"))
			writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		h.serverError(w, "login", err)
		return
	}
	if !auth.CheckPasswordHash(u.PasswordHash, req.Password) {
		auditAuth(req.Username, "login", "failure", "wrong_password")
		_ = h.recordAudit(r, authEvent(u.ID, "auth.login", "denied", "invalid_credentials"))
		writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	resp, err := h.issueTokens(r.Context(), u)
	if err != nil {
		h.serverError(w, "login", err)
		return
	}
	auditAuth(u.Username, "login", "success", "issued")
	if err := h.recordAudit(r, authEvent(u.ID, "auth.login", "success", "issued")); err != nil {
		h.serverError(w, "login", err)
		return
	}
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
		// A replayed token is a compromise signal, not a client error: it means
		// a token already consumed by the legitimate holder was presented
		// again, and the whole family has just been revoked. It is recorded
		// under its own outcome so it can be alerted on, and the response stays
		// uniform so the caller learns nothing.
		detail := "token_rejected"
		if errors.Is(err, auth.ErrRefreshTokenReused) {
			detail = "replay_detected"
		}
		_ = h.recordAudit(r, authEvent(u.ID, "auth.refresh", "denied", detail))
		writeJSONError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	access, err := h.jwt.GenerateAccessTokenWithPermissions(u.ID, u.WorkspaceID, auth.RoleFor(u), rbac.PermissionsForRole(auth.RoleFor(u)))
	if err != nil {
		h.serverError(w, "refresh", err)
		return
	}
	auditAuth(u.Username, "refresh", "success", "rotated")
	if err := h.recordAudit(r, authEvent(u.ID, "auth.refresh", "success", "rotated")); err != nil {
		h.serverError(w, "refresh", err)
		return
	}
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
	if err := h.recordAudit(r, authEvent("", "auth.logout", "success", "revoked")); err != nil {
		h.serverError(w, "logout", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Me returns the authenticated caller's profile: only safe profile fields. No
// password hash, no refresh-token material, and no internal database fields
// are ever exposed. The role is the identity's persisted role from the
// database (via token claims). The route is registered under an explicit
// authorization rule (GET /api/me), so reaching this handler means the
// verified claims already carry the permission.
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
	role := auth.RoleFor(u)
	writeJSON(w, http.StatusOK, &meResponse{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Role:        string(role),
		IsFounder:   u.IsFounder,
		WorkspaceID: u.WorkspaceID,
	})
}

// issueTokens mints an access token carrying the identity's role and the
// explicit role-based permissions, plus a new refresh token backed by the
// refresh store.
func (h *AuthHandler) issueTokens(ctx context.Context, u *auth.UserRecord) (*tokenResponse, error) {
	role := auth.RoleFor(u)
	access, err := h.jwt.GenerateAccessTokenWithPermissions(u.ID, u.WorkspaceID, role, rbac.PermissionsForRole(role))
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
	// Reject trailing data. Without this a body carrying a second JSON value
	// after the object we parsed is accepted, which lets a request smuggle
	// content past anything that inspects only the parsed object and makes the
	// accepted body differ from the one the client claims to have sent.
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
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

// recordAudit persists one security-relevant authentication event to the
// append-only audit chain and returns any persistence failure.
//
// Correlation identifiers come from the request, but only when they parse as
// UUIDs: a hostile or malformed header is dropped rather than stored, so the
// audit record cannot be used to smuggle attacker-controlled text into a
// tamper-evident log.
//
// These events are organization-level (WorkspaceID nil): authentication
// happens before a workspace context is established, and the login history of
// an identity must remain reachable from whichever workspace it later lands in.
func (h *AuthHandler) recordAudit(r *http.Request, rec audit.Record) error {
	if h.auditSink == nil {
		return errAuditUnavailable
	}
	cv := middleware.ExtractContextValues(r)
	if id, err := uuid.Parse(cv.TraceID); err == nil {
		rec.TraceID = id
	}
	if id, err := uuid.Parse(cv.SpanID); err == nil {
		rec.SpanID = id
	}
	if _, err := h.auditSink.Append(r.Context(), rec); err != nil {
		logger.NewEntry("audit-persist-failed").SetLevel("error").
			With("event_type", rec.EventType).
			With("outcome", rec.Outcome).
			WithError(err).Log()
		return err
	}
	return nil
}

// authEvent is the audit record shape shared by the authentication endpoints.
// Only the identity's UUID is recorded, never the username or any credential:
// the password and the tokens are not passed in at all, and
// audit.RedactDetails would strip them if they were.
func authEvent(actorID string, eventType, outcome, detail string) audit.Record {
	return audit.Record{
		EventType:  eventType,
		ActorType:  "user",
		ActorID:    parseActorID(actorID),
		TargetType: "user",
		TargetID:   parseActorID(actorID),
		Outcome:    outcome,
		Principle:  "Security by Design",
		Details:    map[string]any{"detail": detail},
	}
}

// parseActorID converts a stored identity id into the UUID the audit schema
// uses. An id that does not parse yields the nil UUID rather than an error: the
// event is still worth recording, and failing an audit write over a malformed
// identifier would be the worse outcome.
func parseActorID(id string) uuid.UUID {
	if id == "" {
		return uuid.Nil
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

func auditAuth(actorID, action, outcome, detail string) {
	logger.NewEntry("auth-audit").
		With("actor_id", actorID).
		With("action", action).
		With("outcome", outcome).
		With("detail", detail).
		Log()
}
