package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/department"
	logger "austro-os/internal/log"
	"austro-os/internal/middleware"

	"github.com/google/uuid"
)

const (
	departmentNameMaxRunes = 100
)

// DepartmentHandler exposes the department endpoints:
// POST /departments, GET /departments, GET /departments/{id}, PATCH /departments/{id}, DELETE /departments/{id}
// Workspace is taken from verified claims, never from client.
type DepartmentHandler struct {
	svc       DepartmentService
	auditSink audit.Sink
}

type DepartmentService interface {
	Create(ctx context.Context, workspaceID uuid.UUID, name string) (*department.Department, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*department.Department, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]*department.Department, error)
	ListPage(ctx context.Context, workspaceID uuid.UUID, q department.ListQuery) (department.Page, error)
	Update(ctx context.Context, workspaceID, id uuid.UUID, name string) (*department.Department, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

func NewDepartmentHandler(svc DepartmentService) *DepartmentHandler {
	return &DepartmentHandler{svc: svc}
}

func (h *DepartmentHandler) SetAuditSink(s audit.Sink) *DepartmentHandler {
	h.auditSink = s
	return h
}

func (h *DepartmentHandler) record(r *http.Request, claims *auth.Claims, action string, deptID, ws uuid.UUID, outcome, detail string) {
	if h.auditSink == nil {
		return
	}
	rec := audit.Record{
		EventType:  "department." + strings.TrimSuffix(action, "-department"),
		ActorType:  "user",
		ActorID:    parseActorID(claims.ID),
		TargetType: "department",
		TargetID:   deptID,
		Outcome:    outcome,
		Principle:  "Security by Design",
	}
	if ws != uuid.Nil {
		wsCopy := ws
		rec.WorkspaceID = &wsCopy
	}
	if detail != "" {
		rec.Details = map[string]any{"detail": detail}
	}
	cv := middleware.ExtractContextValues(r)
	if id, err := uuid.Parse(cv.TraceID); err == nil {
		rec.TraceID = id
	}
	if _, err := h.auditSink.Append(r.Context(), rec); err != nil {
		logger.NewEntry("department-audit-persist-failed").SetLevel("error").With("event_type", rec.EventType).WithError(err).Log()
	}
}

func (h *DepartmentHandler) scope(w http.ResponseWriter, r *http.Request, action string) (*auth.Claims, uuid.UUID, bool) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, uuid.Nil, false
	}
	binding, err := WorkspaceFromClaims(claims)
	if err != nil {
		h.record(r, claims, action, uuid.Nil, uuid.Nil, "denied", "no_workspace_context")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	ws, err := uuid.Parse(binding)
	if err != nil {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	return claims, ws, true
}

func (h *DepartmentHandler) fail(w http.ResponseWriter, r *http.Request, claims *auth.Claims, action string, id uuid.UUID, err error) {
	switch {
	case errors.Is(err, department.ErrNotFound):
		h.record(r, claims, action, id, uuid.Nil, "failed", "not_found")
		writeJSONError(w, http.StatusNotFound, "department not found")
	case errors.Is(err, department.ErrWorkspaceMismatch):
		h.record(r, claims, action, id, uuid.Nil, "denied", "workspace_mismatch")
		writeJSONError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, department.ErrNameTaken):
		h.record(r, claims, action, id, uuid.Nil, "failed", "name_taken")
		writeJSONError(w, http.StatusConflict, "department name already exists")
	case errors.Is(err, department.ErrInvalidInput):
		h.record(r, claims, action, id, uuid.Nil, "failed", "invalid_input")
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		logger.NewEntry("department-handler-error").SetLevel("error").With("action", action).WithError(err).Log()
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

type createDepartmentRequest struct {
	Name string `json:"name"`
}

type updateDepartmentRequest struct {
	Name *string `json:"name"`
}

type departmentResponse struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type departmentPage struct {
	Departments []departmentResponse `json:"departments"`
	NextCursor  string               `json:"next_cursor,omitempty"`
	Limit       int                  `json:"limit"`
}

func (h *DepartmentHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "list-department")
	if !ok {
		return
	}
	q, ok := parseDepartmentQuery(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListPage(r.Context(), ws, q)
	if err != nil {
		h.fail(w, r, claims, "list-department", uuid.Nil, err)
		return
	}
	h.record(r, claims, "list-department", uuid.Nil, ws, "success", "")
	writeJSON(w, http.StatusOK, toDepartmentPage(page))
}

func (h *DepartmentHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "create-department")
	if !ok {
		return
	}
	var req createDepartmentRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "create-department", uuid.Nil, ws, "failed", "invalid_body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		h.record(r, claims, "create-department", uuid.Nil, ws, "failed", "name_required")
		return
	}
	if utf8.RuneCountInString(name) > departmentNameMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "name is too long")
		h.record(r, claims, "create-department", uuid.Nil, ws, "failed", "name_too_long")
		return
	}
	d, err := h.svc.Create(r.Context(), ws, name)
	if err != nil {
		h.fail(w, r, claims, "create-department", uuid.Nil, err)
		return
	}
	h.record(r, claims, "create-department", d.ID, ws, "success", "")
	writeJSON(w, http.StatusCreated, toDepartmentResponse(d))
}

func (h *DepartmentHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "get-department")
	if !ok {
		return
	}
	id, ok := departmentIDFromPath(w, r)
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		h.fail(w, r, claims, "get-department", id, err)
		return
	}
	h.record(r, claims, "get-department", d.ID, ws, "success", "")
	writeJSON(w, http.StatusOK, toDepartmentResponse(d))
}

func (h *DepartmentHandler) Update(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "update-department")
	if !ok {
		return
	}
	id, ok := departmentIDFromPath(w, r)
	if !ok {
		return
	}
	var req updateDepartmentRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "update-department", id, ws, "failed", "invalid_body")
		return
	}
	if req.Name == nil {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		h.record(r, claims, "update-department", id, ws, "failed", "name_required")
		return
	}
	name := strings.TrimSpace(*req.Name)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name must not be blank")
		h.record(r, claims, "update-department", id, ws, "failed", "name_blank")
		return
	}
	if utf8.RuneCountInString(name) > departmentNameMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "name is too long")
		h.record(r, claims, "update-department", id, ws, "failed", "name_too_long")
		return
	}
	d, err := h.svc.Update(r.Context(), ws, id, name)
	if err != nil {
		h.fail(w, r, claims, "update-department", id, err)
		return
	}
	h.record(r, claims, "update-department", d.ID, ws, "success", "")
	writeJSON(w, http.StatusOK, toDepartmentResponse(d))
}

func (h *DepartmentHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "delete-department")
	if !ok {
		return
	}
	id, ok := departmentIDFromPath(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), ws, id); err != nil {
		h.fail(w, r, claims, "delete-department", id, err)
		return
	}
	h.record(r, claims, "delete-department", id, ws, "success", "")
	w.WriteHeader(http.StatusNoContent)
}

func parseDepartmentQuery(w http.ResponseWriter, r *http.Request) (department.ListQuery, bool) {
	var q department.ListQuery
	known := map[string]bool{"limit": true, "cursor": true}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed query string")
		return q, false
	}
	for key := range values {
		if !known[key] {
			writeJSONError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
			return q, false
		}
	}
	if v := values.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return q, false
		}
		q.Limit = n
	}
	if v := values.Get("cursor"); v != "" {
		c, err := department.DecodeCursor(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "cursor is not valid")
			return q, false
		}
		q.Cursor = c
	}
	q.Normalize()
	return q, true
}

func departmentIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid department id")
		return uuid.Nil, false
	}
	return id, true
}

func toDepartmentResponse(d *department.Department) departmentResponse {
	return departmentResponse{
		ID:          d.ID.String(),
		WorkspaceID: d.WorkspaceID.String(),
		Name:        d.Name,
		CreatedAt:   d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   d.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func toDepartmentPage(p department.Page) departmentPage {
	page := departmentPage{Limit: p.Limit, NextCursor: p.NextCursor}
	page.Departments = make([]departmentResponse, 0, len(p.Departments))
	for _, d := range p.Departments {
		page.Departments = append(page.Departments, toDepartmentResponse(d))
	}
	return page
}

var _ DepartmentService = (*department.Service)(nil)
