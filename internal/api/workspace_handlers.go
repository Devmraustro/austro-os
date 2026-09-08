package api

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"austro-os/internal/auth"
	logger "austro-os/internal/log"
	"austro-os/internal/rbac"
	"austro-os/internal/workspace"

	"github.com/google/uuid"
)

const (
	// workspaceNameMaxRunes bounds a workspace name. Names are user-facing
	// identifiers, so the bound is stated in runes rather than bytes.
	workspaceNameMaxRunes = 100
)

// WorkspaceHandler exposes the workspace administration endpoints:
//
//   - GET  /workspaces         organization-level listing (founder only)
//   - POST /workspaces         organization-level creation (founder only)
//   - GET  /workspaces/{id}    workspace-scoped read (founder any; admin own only)
//
// It never accepts a workspace context from the client: the caller's role and
// workspace come exclusively from the verified claims attached by RequireAuth,
// and the enforcement point is the deny-by-default authorization middleware.
// The explicit role guards below are defense-in-depth on top of the middleware.
type WorkspaceHandler struct {
	store workspace.Store
}

// NewWorkspaceHandler builds the handler with the workspace store port.
func NewWorkspaceHandler(store workspace.Store) *WorkspaceHandler {
	return &WorkspaceHandler{store: store}
}

// List returns every workspace. Only the founder's organization-level rule
// (GET /workspaces, ScopeNone) reaches this handler; any other caller is
// refused before this code runs (deny-by-default middleware) and is refused
// here again as defense-in-depth.
func (h *WorkspaceHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if claims.Role != rbac.RoleFounder {
		auditWorkspace(claims.ID, "list-workspaces", "denied", "role_insufficient")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	ws, err := h.store.List(r.Context())
	if err != nil {
		h.serverError(w, "list-workspaces", err)
		return
	}
	out := make([]workspaceResponse, 0, len(ws))
	for _, rec := range ws {
		out = append(out, toWorkspaceResponse(rec))
	}
	auditWorkspace(claims.ID, "list-workspaces", "success", "")
	writeJSON(w, http.StatusOK, out)
}

// Create creates a workspace. Founder only (POST /workspaces, ScopeNone). The
// name is required, trimmed and length-bounded; unknown fields and
// client-supplied workspace ids are rejected by strict decoding. A duplicate
// name surfaces as a deterministic 409 conflict.
func (h *WorkspaceHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if claims.Role != rbac.RoleFounder {
		auditWorkspace(claims.ID, "create-workspace", "denied", "role_insufficient")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	var req createWorkspaceRequest
	if !decodeJSON(w, r, &req) {
		auditWorkspace(claims.ID, "create-workspace", "rejected", "malformed_request")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "workspace name is required")
		return
	}
	if utf8.RuneCountInString(name) > workspaceNameMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "workspace name is too long")
		return
	}

	rec, err := h.store.Create(r.Context(), name)
	if err != nil {
		if errors.Is(err, workspace.ErrNameTaken) {
			auditWorkspace(claims.ID, "create-workspace", "conflict", "name_taken")
			writeJSONError(w, http.StatusConflict, "workspace name already exists")
			return
		}
		h.serverError(w, "create-workspace", err)
		return
	}
	auditWorkspace(claims.ID, "create-workspace", "success", rec.ID.String())
	writeJSON(w, http.StatusCreated, toWorkspaceResponse(rec))
}

// Get returns a workspace by id. The founder's organization-level read is
// explicit (ScopeNone rule) and passes the path id through UUID validation. A
// workspace admin reaches this handler only through the ScopePath rule, which
// guarantees the path id equals the claims workspace; the store lookup for an
// admin is still derived from the verified claims, never from the client. A
// workspace member has no rule for this route and is denied.
func (h *WorkspaceHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	switch claims.Role {
	case rbac.RoleFounder:
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid workspace id")
			return
		}
		h.getByID(w, r, claims.ID, id)
	case rbac.RoleWorkspaceAdmin:
		// ScopePath was enforced by the middleware (path workspace must equal
		// the claims workspace). Bind the read to the claims workspace so a
		// diverging path can never drive the query.
		binding, err := WorkspaceFromClaims(claims)
		if err != nil {
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		h.getByID(w, r, claims.ID, uuid.MustParse(binding))
	default:
		// workspace_member (and unsupported roles) carry no rule for this route.
		auditWorkspace(claims.ID, "read-workspace", "denied", "role_insufficient")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
}

func (h *WorkspaceHandler) getByID(w http.ResponseWriter, r *http.Request, actor string, id uuid.UUID) {
	rec, err := h.store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			auditWorkspace(actor, "read-workspace", "not_found", id.String())
			writeJSONError(w, http.StatusNotFound, "workspace not found")
			return
		}
		h.serverError(w, "read-workspace", err)
		return
	}
	auditWorkspace(actor, "read-workspace", "success", id.String())
	writeJSON(w, http.StatusOK, toWorkspaceResponse(rec))
}

// serverError logs the technical failure (with its cause) while exposing only a
// generic message, so no SQL or constraint detail ever leaks to the caller.
func (h *WorkspaceHandler) serverError(w http.ResponseWriter, action string, err error) {
	logger.NewEntry("workspace-handler-error").SetLevel("error").With("action", action).WithError(err).Log()
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}

// auditWorkspace records a workspace administration attempt for the audit trail.
func auditWorkspace(actorID, action, outcome, detail string) {
	logger.NewEntry("workspace-audit").
		With("actor_id", actorID).
		With("action", action).
		With("outcome", outcome).
		With("detail", detail).
		Log()
}

type createWorkspaceRequest struct {
	Name string `json:"name"`
}

type workspaceResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toWorkspaceResponse(w *workspace.Workspace) workspaceResponse {
	return workspaceResponse{
		ID:        w.ID.String(),
		Name:      w.Name,
		CreatedAt: w.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: w.UpdatedAt.UTC().Format(time.RFC3339),
	}
}
