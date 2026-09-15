package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/memory"
	"austro-os/internal/middleware"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
)

// MemoryHandler exposes the deliberately small, key-based Memory contract:
//
//	GET /memory/{layer}/{key}  read one cell
//	PUT /memory/{layer}/{key}  create or replace one cell
//
// The API exposes only workspace-bound layers. Organizational memory remains an
// internal domain capability because it has no caller workspace and granting a
// tenant HTTP access to it would conflict with the approved deny-by-default
// model. Layer names are lower-case and exact: session, longterm, workspace.
// The server derives the workspace and actor exclusively from verified claims.
type MemoryHandler struct {
	bank      *memory.Bank
	auditSink audit.Sink
}

// NewMemoryHandler wires the workspace-scoped facade to the HTTP boundary.
func NewMemoryHandler(bank *memory.Bank) *MemoryHandler {
	return &MemoryHandler{bank: bank}
}

// SetAuditSink attaches the existing persistent audit chain for rejected
// requests that fail before the Bank boundary is reached.
func (h *MemoryHandler) SetAuditSink(sink audit.Sink) *MemoryHandler {
	h.auditSink = sink
	return h
}

type memoryWriteRequest struct {
	Value      *string `json:"value"`
	TTLSeconds *int64  `json:"ttl_seconds"`
}

type memoryResponse struct {
	Layer       string `json:"layer"`
	Key         string `json:"key"`
	Value       string `json:"value"`
	WorkspaceID string `json:"workspace_id"`
	TTLSeconds  *int64 `json:"ttl_seconds,omitempty"`
}

// layerForHTTP is intentionally narrower than memory.ValidLayer. The domain
// retains its four-layer internal model, but the approved tenant API has only
// the three workspace-bound values. In particular, org, organizational and the
// upper-case SESSION spelling are not accepted aliases.
func layerForHTTP(raw string) (memory.MemoryLayer, bool) {
	switch raw {
	case "session":
		return memory.LayerSession, true
	case "longterm":
		return memory.LayerLongTerm, true
	case "workspace":
		return memory.LayerWorkspace, true
	default:
		return 0, false
	}
}

func (h *MemoryHandler) scope(w http.ResponseWriter, r *http.Request, action string) (*auth.Claims, uuid.UUID, bool) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, uuid.Nil, false
	}
	if !rbac.ValidRole(claims.Role) {
		h.failAudit(r, claims, action, "unsupported_role")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	if _, err := uuid.Parse(claims.ID); err != nil {
		h.failAudit(r, claims, action, "invalid_actor_claim")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	binding, err := WorkspaceFromClaims(claims)
	if err != nil {
		h.failAudit(r, claims, action, "no_workspace_context")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	workspaceID, err := uuid.Parse(binding)
	if err != nil {
		h.failAudit(r, claims, action, "invalid_workspace_claim")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	return claims, workspaceID, true
}

// Read returns one memory cell. A missing cell is a normal 404 and transport or
// audit failures are deliberately reduced to a generic 500 response.
func (h *MemoryHandler) Read(w http.ResponseWriter, r *http.Request) {
	claims, workspaceID, ok := h.scope(w, r, "read")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		h.failAudit(r, claims, "read", "unknown_parameter")
		writeJSONError(w, http.StatusBadRequest, "memory read does not accept query parameters")
		return
	}
	layer, key, ok := h.path(w, r, claims, "read")
	if !ok {
		return
	}

	cv := middleware.ExtractContextValues(r)
	ctx := memory.WithActor(memory.WithTrace(r.Context(), cv.TraceID, cv.SpanID), claims.ID)
	value, err := h.bank.Read(ctx, workspaceID, layer, key)
	if err != nil {
		h.memoryError(w, r, claims, "read", layer, key, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryResponse{
		Layer: layerName(layer), Key: key, Value: string(value),
		WorkspaceID: workspaceID.String(),
	})
}

// Write creates or replaces one cell. PUT is idempotent for a given
// workspace/layer/key tuple, and the workspace cannot be supplied by the body.
func (h *MemoryHandler) Write(w http.ResponseWriter, r *http.Request) {
	claims, workspaceID, ok := h.scope(w, r, "write")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		h.failAudit(r, claims, "write", "unknown_parameter")
		writeJSONError(w, http.StatusBadRequest, "memory write does not accept query parameters")
		return
	}
	layer, key, ok := h.path(w, r, claims, "write")
	if !ok {
		return
	}
	var req memoryWriteRequest
	if !decodeJSON(w, r, &req) {
		h.failAudit(r, claims, "write", "malformed_body")
		return
	}
	if req.Value == nil {
		h.failAudit(r, claims, "write", "missing_value")
		writeJSONError(w, http.StatusBadRequest, "value is required")
		return
	}

	ttl := memory.DefaultTTL
	if req.TTLSeconds != nil {
		seconds := *req.TTLSeconds
		if seconds < 0 || seconds > int64(memory.MaxTTL/time.Second) {
			h.failAudit(r, claims, "write", "invalid_ttl")
			writeJSONError(w, http.StatusBadRequest, "ttl_seconds is outside the supported range")
			return
		}
		ttl = time.Duration(seconds) * time.Second
	}

	cv := middleware.ExtractContextValues(r)
	ctx := memory.WithActor(memory.WithTrace(r.Context(), cv.TraceID, cv.SpanID), claims.ID)
	if err := h.bank.Write(ctx, workspaceID, layer, key, []byte(*req.Value), ttl); err != nil {
		h.memoryError(w, r, claims, "write", layer, key, err)
		return
	}
	effectiveTTL := int64(ttl / time.Second)
	writeJSON(w, http.StatusOK, memoryResponse{
		Layer: layerName(layer), Key: key, Value: *req.Value,
		WorkspaceID: workspaceID.String(), TTLSeconds: &effectiveTTL,
	})
}

func (h *MemoryHandler) path(w http.ResponseWriter, r *http.Request, claims *auth.Claims, action string) (memory.MemoryLayer, string, bool) {
	rawLayer := r.PathValue("layer")
	layer, ok := layerForHTTP(rawLayer)
	if !ok {
		h.failAudit(r, claims, action, "invalid_layer")
		writeJSONError(w, http.StatusBadRequest, "invalid memory layer")
		return 0, "", false
	}
	key := r.PathValue("key")
	if strings.TrimSpace(key) == "" {
		h.failAudit(r, claims, action, "invalid_key")
		writeJSONError(w, http.StatusBadRequest, "invalid memory key")
		return 0, "", false
	}
	return layer, key, true
}

func layerName(layer memory.MemoryLayer) string {
	switch layer {
	case memory.LayerSession:
		return "session"
	case memory.LayerLongTerm:
		return "longterm"
	case memory.LayerWorkspace:
		return "workspace"
	default:
		return "unknown"
	}
}

func (h *MemoryHandler) memoryError(w http.ResponseWriter, r *http.Request, claims *auth.Claims,
	action string, layer memory.MemoryLayer, key string, err error) {
	switch {
	case errors.Is(err, memory.ErrNotFound):
		h.failAudit(r, claims, action, "not_found")
		writeJSONError(w, http.StatusNotFound, "memory entry not found")
	case errors.Is(err, memory.ErrInvalidLayer),
		errors.Is(err, memory.ErrInvalidKey),
		errors.Is(err, memory.ErrValueTooLarge),
		errors.Is(err, memory.ErrSecretRejected),
		errors.Is(err, memory.ErrInvalidTTL):
		h.failAudit(r, claims, action, "invalid_request")
		writeJSONError(w, http.StatusBadRequest, "invalid memory request")
	case errors.Is(err, memory.ErrRateLimited):
		h.failAudit(r, claims, action, "rate_limited")
		writeJSONError(w, http.StatusTooManyRequests, "memory write rate limit exceeded")
	default:
		h.failAudit(r, claims, action, "internal_error")
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *MemoryHandler) failAudit(r *http.Request, claims *auth.Claims, action, detail string) {
	if h.auditSink == nil || claims == nil {
		return
	}
	actorID, err := uuid.Parse(claims.ID)
	if err != nil {
		return
	}
	cv := middleware.ExtractContextValues(r)
	rec := audit.Record{
		EventType:  "memory." + action,
		ActorType:  "user",
		ActorID:    actorID,
		TargetType: "memory_route",
		Outcome:    "failed",
		Principle:  "Security by Design",
		Details:    map[string]any{"detail": detail},
	}
	if id, err := uuid.Parse(cv.TraceID); err == nil {
		rec.TraceID = id
	}
	if id, err := uuid.Parse(cv.SpanID); err == nil {
		rec.SpanID = id
	}
	if _, err := h.auditSink.Append(r.Context(), rec); err != nil {
		// Denial/error reporting must remain deterministic even if the audit
		// database is unavailable; the durable mutation path fails closed in
		// the Bank's stronger sink, while this is only diagnostic evidence for
		// a request rejected before a cell operation.
		return
	}
}
