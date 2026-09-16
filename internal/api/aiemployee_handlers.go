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

	"austro-os/internal/aiemployee"
	"austro-os/internal/audit"
	"austro-os/internal/auth"
	logger "austro-os/internal/log"
	"austro-os/internal/middleware"

	"github.com/google/uuid"
)

const (
	employeeNameMaxRunes = 100
	employeeRoleMaxRunes = 100
)

type AIEmployeeHandler struct {
	svc       AIEmployeeService
	auditSink audit.Sink
}

type AIEmployeeService interface {
	Create(ctx context.Context, workspaceID, teamID uuid.UUID, name, role string, capabilities []string) (*aiemployee.AIEmployee, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*aiemployee.AIEmployee, error)
	List(ctx context.Context, workspaceID uuid.UUID, teamID *uuid.UUID) ([]*aiemployee.AIEmployee, error)
	ListPage(ctx context.Context, workspaceID uuid.UUID, q aiemployee.ListQuery) (aiemployee.Page, error)
	Update(ctx context.Context, workspaceID, id uuid.UUID, name, role *string, capabilities []string, teamID *uuid.UUID, currentTaskID *uuid.UUID) (*aiemployee.AIEmployee, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

func NewAIEmployeeHandler(svc AIEmployeeService) *AIEmployeeHandler {
	return &AIEmployeeHandler{svc: svc}
}

func (h *AIEmployeeHandler) SetAuditSink(s audit.Sink) *AIEmployeeHandler {
	h.auditSink = s
	return h
}

func (h *AIEmployeeHandler) record(r *http.Request, claims *auth.Claims, action string, empID, ws uuid.UUID, outcome, detail string) {
	if h.auditSink == nil {
		return
	}
	rec := audit.Record{
		EventType:  "ai_employee." + strings.TrimSuffix(action, "-ai-employee"),
		ActorType:  "user",
		ActorID:    parseActorID(claims.ID),
		TargetType: "ai_employee",
		TargetID:   empID,
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
		logger.NewEntry("ai-employee-audit-persist-failed").SetLevel("error").With("event_type", rec.EventType).WithError(err).Log()
	}
}

func (h *AIEmployeeHandler) scope(w http.ResponseWriter, r *http.Request, action string) (*auth.Claims, uuid.UUID, bool) {
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

func (h *AIEmployeeHandler) fail(w http.ResponseWriter, r *http.Request, claims *auth.Claims, action string, id uuid.UUID, err error) {
	switch {
	case errors.Is(err, aiemployee.ErrNotFound):
		h.record(r, claims, action, id, uuid.Nil, "failed", "not_found")
		writeJSONError(w, http.StatusNotFound, "ai employee not found")
	case errors.Is(err, aiemployee.ErrWorkspaceMismatch):
		h.record(r, claims, action, id, uuid.Nil, "denied", "workspace_mismatch")
		writeJSONError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, aiemployee.ErrNameTaken):
		h.record(r, claims, action, id, uuid.Nil, "failed", "name_taken")
		writeJSONError(w, http.StatusConflict, "ai employee name already exists")
	case errors.Is(err, aiemployee.ErrInvalidInput):
		h.record(r, claims, action, id, uuid.Nil, "failed", "invalid_input")
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		logger.NewEntry("ai-employee-handler-error").SetLevel("error").With("action", action).WithError(err).Log()
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

type createAIEmployeeRequest struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	TeamID       string   `json:"team_id"`
	Capabilities []string `json:"capabilities"`
}

type updateAIEmployeeRequest struct {
	Name          *string  `json:"name"`
	Role          *string  `json:"role"`
	TeamID        *string  `json:"team_id"`
	Capabilities  []string `json:"capabilities"`
	CurrentTaskID *string  `json:"current_task_id"`
}

type aiEmployeeResponse struct {
	ID            string   `json:"id"`
	TeamID        string   `json:"team_id"`
	DepartmentID  string   `json:"department_id"`
	WorkspaceID   string   `json:"workspace_id"`
	Name          string   `json:"name"`
	Role          string   `json:"role"`
	Capabilities  []string `json:"capabilities"`
	CurrentTaskID string   `json:"current_task_id,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

type aiEmployeePage struct {
	Employees  []aiEmployeeResponse `json:"ai_employees"`
	NextCursor string               `json:"next_cursor,omitempty"`
	Limit      int                  `json:"limit"`
}

func (h *AIEmployeeHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "list-ai-employee")
	if !ok {
		return
	}
	q, ok := parseAIEmployeeQuery(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListPage(r.Context(), ws, q)
	if err != nil {
		h.fail(w, r, claims, "list-ai-employee", uuid.Nil, err)
		return
	}
	h.record(r, claims, "list-ai-employee", uuid.Nil, ws, "success", "")
	writeJSON(w, http.StatusOK, toAIEmployeePage(page))
}

func (h *AIEmployeeHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "create-ai-employee")
	if !ok {
		return
	}
	var req createAIEmployeeRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "create-ai-employee", uuid.Nil, ws, "failed", "invalid_body")
		return
	}
	name := strings.TrimSpace(req.Name)
	role := strings.TrimSpace(req.Role)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		h.record(r, claims, "create-ai-employee", uuid.Nil, ws, "failed", "name_required")
		return
	}
	if role == "" {
		writeJSONError(w, http.StatusBadRequest, "role is required")
		h.record(r, claims, "create-ai-employee", uuid.Nil, ws, "failed", "role_required")
		return
	}
	if utf8.RuneCountInString(name) > employeeNameMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "name is too long")
		h.record(r, claims, "create-ai-employee", uuid.Nil, ws, "failed", "name_too_long")
		return
	}
	if utf8.RuneCountInString(role) > employeeRoleMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "role is too long")
		h.record(r, claims, "create-ai-employee", uuid.Nil, ws, "failed", "role_too_long")
		return
	}
	teamID, err := uuid.Parse(strings.TrimSpace(req.TeamID))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "team_id must be a UUID")
		h.record(r, claims, "create-ai-employee", uuid.Nil, ws, "failed", "invalid_team_id")
		return
	}
	emp, err := h.svc.Create(r.Context(), ws, teamID, name, role, req.Capabilities)
	if err != nil {
		h.fail(w, r, claims, "create-ai-employee", uuid.Nil, err)
		return
	}
	h.record(r, claims, "create-ai-employee", emp.ID, ws, "success", "")
	writeJSON(w, http.StatusCreated, toAIEmployeeResponse(emp))
}

func (h *AIEmployeeHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "get-ai-employee")
	if !ok {
		return
	}
	id, ok := aiEmployeeIDFromPath(w, r)
	if !ok {
		return
	}
	emp, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		h.fail(w, r, claims, "get-ai-employee", id, err)
		return
	}
	h.record(r, claims, "get-ai-employee", emp.ID, ws, "success", "")
	writeJSON(w, http.StatusOK, toAIEmployeeResponse(emp))
}

func (h *AIEmployeeHandler) Update(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "update-ai-employee")
	if !ok {
		return
	}
	id, ok := aiEmployeeIDFromPath(w, r)
	if !ok {
		return
	}
	var req updateAIEmployeeRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "update-ai-employee", id, ws, "failed", "invalid_body")
		return
	}
	if req.Name == nil && req.Role == nil && req.TeamID == nil && req.Capabilities == nil && req.CurrentTaskID == nil {
		writeJSONError(w, http.StatusBadRequest, "at least one field is required")
		h.record(r, claims, "update-ai-employee", id, ws, "failed", "empty_update")
		return
	}
	var name *string
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeJSONError(w, http.StatusBadRequest, "name must not be blank")
			h.record(r, claims, "update-ai-employee", id, ws, "failed", "name_blank")
			return
		}
		if utf8.RuneCountInString(trimmed) > employeeNameMaxRunes {
			writeJSONError(w, http.StatusBadRequest, "name is too long")
			h.record(r, claims, "update-ai-employee", id, ws, "failed", "name_too_long")
			return
		}
		name = &trimmed
	}
	var role *string
	if req.Role != nil {
		trimmed := strings.TrimSpace(*req.Role)
		if trimmed == "" {
			writeJSONError(w, http.StatusBadRequest, "role must not be blank")
			h.record(r, claims, "update-ai-employee", id, ws, "failed", "role_blank")
			return
		}
		if utf8.RuneCountInString(trimmed) > employeeRoleMaxRunes {
			writeJSONError(w, http.StatusBadRequest, "role is too long")
			h.record(r, claims, "update-ai-employee", id, ws, "failed", "role_too_long")
			return
		}
		role = &trimmed
	}
	var teamID *uuid.UUID
	if req.TeamID != nil {
		parsed, err := uuid.Parse(strings.TrimSpace(*req.TeamID))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "team_id must be a UUID")
			h.record(r, claims, "update-ai-employee", id, ws, "failed", "invalid_team_id")
			return
		}
		teamID = &parsed
	}
	var currentTaskID *uuid.UUID
	if req.CurrentTaskID != nil {
		trimmed := strings.TrimSpace(*req.CurrentTaskID)
		if trimmed == "" {
			// clear assignment
			nilUUID := uuid.Nil
			currentTaskID = &nilUUID
		} else {
			parsed, err := uuid.Parse(trimmed)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "current_task_id must be a UUID")
				h.record(r, claims, "update-ai-employee", id, ws, "failed", "invalid_task_id")
				return
			}
			currentTaskID = &parsed
		}
	}
	emp, err := h.svc.Update(r.Context(), ws, id, name, role, req.Capabilities, teamID, currentTaskID)
	if err != nil {
		h.fail(w, r, claims, "update-ai-employee", id, err)
		return
	}
	h.record(r, claims, "update-ai-employee", emp.ID, ws, "success", "")
	writeJSON(w, http.StatusOK, toAIEmployeeResponse(emp))
}

func (h *AIEmployeeHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "delete-ai-employee")
	if !ok {
		return
	}
	id, ok := aiEmployeeIDFromPath(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), ws, id); err != nil {
		h.fail(w, r, claims, "delete-ai-employee", id, err)
		return
	}
	h.record(r, claims, "delete-ai-employee", id, ws, "success", "")
	w.WriteHeader(http.StatusNoContent)
}

func parseAIEmployeeQuery(w http.ResponseWriter, r *http.Request) (aiemployee.ListQuery, bool) {
	var q aiemployee.ListQuery
	known := map[string]bool{"limit": true, "cursor": true, "team_id": true}
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
		c, err := aiemployee.DecodeCursor(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "cursor is not valid")
			return q, false
		}
		q.Cursor = c
	}
	if v := values.Get("team_id"); v != "" {
		id, err := uuid.Parse(strings.TrimSpace(v))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "team_id must be a UUID")
			return q, false
		}
		q.TeamID = &id
	}
	q.Normalize()
	return q, true
}

func aiEmployeeIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid ai employee id")
		return uuid.Nil, false
	}
	return id, true
}

func toAIEmployeeResponse(e *aiemployee.AIEmployee) aiEmployeeResponse {
	resp := aiEmployeeResponse{
		ID:           e.ID.String(),
		TeamID:       e.TeamID.String(),
		DepartmentID: e.DepartmentID.String(),
		WorkspaceID:  e.WorkspaceID.String(),
		Name:         e.Name,
		Role:         e.Role,
		Capabilities: e.Capabilities,
		CreatedAt:    e.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    e.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if e.Capabilities == nil {
		resp.Capabilities = []string{}
	}
	if e.CurrentTaskID != nil {
		resp.CurrentTaskID = e.CurrentTaskID.String()
	}
	return resp
}

func toAIEmployeePage(p aiemployee.Page) aiEmployeePage {
	page := aiEmployeePage{Limit: p.Limit, NextCursor: p.NextCursor}
	page.Employees = make([]aiEmployeeResponse, 0, len(p.Employees))
	for _, e := range p.Employees {
		page.Employees = append(page.Employees, toAIEmployeeResponse(e))
	}
	return page
}

var _ AIEmployeeService = (*aiemployee.Service)(nil)
