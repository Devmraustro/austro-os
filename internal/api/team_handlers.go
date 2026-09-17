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
	logger "austro-os/internal/log"
	"austro-os/internal/middleware"
	"austro-os/internal/team"

	"github.com/google/uuid"
)

const teamNameMaxRunes = 100

type TeamHandler struct {
	svc       TeamService
	auditSink audit.Sink
}

type TeamService interface {
	Create(ctx context.Context, workspaceID, departmentID uuid.UUID, name string) (*team.Team, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*team.Team, error)
	List(ctx context.Context, workspaceID uuid.UUID, departmentID *uuid.UUID) ([]*team.Team, error)
	ListPage(ctx context.Context, workspaceID uuid.UUID, q team.ListQuery) (team.Page, error)
	Update(ctx context.Context, workspaceID, id uuid.UUID, name string, departmentID *uuid.UUID) (*team.Team, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

func NewTeamHandler(svc TeamService) *TeamHandler {
	return &TeamHandler{svc: svc}
}

func (h *TeamHandler) SetAuditSink(s audit.Sink) *TeamHandler {
	h.auditSink = s
	return h
}

func (h *TeamHandler) record(r *http.Request, claims *auth.Claims, action string, teamID, ws uuid.UUID, outcome, detail string) {
	if h.auditSink == nil {
		return
	}
	rec := audit.Record{
		EventType:  "team." + strings.TrimSuffix(action, "-team"),
		ActorType:  "user",
		ActorID:    parseActorID(claims.ID),
		TargetType: "team",
		TargetID:   teamID,
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
		logger.NewEntry("team-audit-persist-failed").SetLevel("error").With("event_type", rec.EventType).WithError(err).Log()
	}
}

func (h *TeamHandler) scope(w http.ResponseWriter, r *http.Request, action string) (*auth.Claims, uuid.UUID, bool) {
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

func (h *TeamHandler) fail(w http.ResponseWriter, r *http.Request, claims *auth.Claims, action string, id uuid.UUID, err error) {
	switch {
	case errors.Is(err, team.ErrNotFound):
		h.record(r, claims, action, id, uuid.Nil, "failed", "not_found")
		writeJSONError(w, http.StatusNotFound, "team not found")
	case errors.Is(err, team.ErrWorkspaceMismatch):
		h.record(r, claims, action, id, uuid.Nil, "denied", "workspace_mismatch")
		writeJSONError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, team.ErrNameTaken):
		h.record(r, claims, action, id, uuid.Nil, "failed", "name_taken")
		writeJSONError(w, http.StatusConflict, "team name already exists")
	case errors.Is(err, team.ErrInvalidInput):
		h.record(r, claims, action, id, uuid.Nil, "failed", "invalid_input")
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		logger.NewEntry("team-handler-error").SetLevel("error").With("action", action).WithError(err).Log()
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

type createTeamRequest struct {
	Name         string `json:"name"`
	DepartmentID string `json:"department_id"`
}

type updateTeamRequest struct {
	Name         *string `json:"name"`
	DepartmentID *string `json:"department_id"`
}

type teamResponse struct {
	ID           string `json:"id"`
	DepartmentID string `json:"department_id"`
	WorkspaceID  string `json:"workspace_id"`
	Name         string `json:"name"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type teamPage struct {
	Teams      []teamResponse `json:"teams"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Limit      int            `json:"limit"`
}

func (h *TeamHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "list-team")
	if !ok {
		return
	}
	q, ok := parseTeamQuery(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListPage(r.Context(), ws, q)
	if err != nil {
		h.fail(w, r, claims, "list-team", uuid.Nil, err)
		return
	}
	h.record(r, claims, "list-team", uuid.Nil, ws, "success", "")
	writeJSON(w, http.StatusOK, toTeamPage(page))
}

func (h *TeamHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "create-team")
	if !ok {
		return
	}
	var req createTeamRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "create-team", uuid.Nil, ws, "failed", "invalid_body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		h.record(r, claims, "create-team", uuid.Nil, ws, "failed", "name_required")
		return
	}
	if utf8.RuneCountInString(name) > teamNameMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "name is too long")
		h.record(r, claims, "create-team", uuid.Nil, ws, "failed", "name_too_long")
		return
	}
	deptID, err := uuid.Parse(strings.TrimSpace(req.DepartmentID))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "department_id must be a UUID")
		h.record(r, claims, "create-team", uuid.Nil, ws, "failed", "invalid_department_id")
		return
	}
	t, err := h.svc.Create(r.Context(), ws, deptID, name)
	if err != nil {
		h.fail(w, r, claims, "create-team", uuid.Nil, err)
		return
	}
	h.record(r, claims, "create-team", t.ID, ws, "success", "")
	writeJSON(w, http.StatusCreated, toTeamResponse(t))
}

func (h *TeamHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "get-team")
	if !ok {
		return
	}
	id, ok := teamIDFromPath(w, r)
	if !ok {
		return
	}
	t, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		h.fail(w, r, claims, "get-team", id, err)
		return
	}
	h.record(r, claims, "get-team", t.ID, ws, "success", "")
	writeJSON(w, http.StatusOK, toTeamResponse(t))
}

func (h *TeamHandler) Update(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "update-team")
	if !ok {
		return
	}
	id, ok := teamIDFromPath(w, r)
	if !ok {
		return
	}
	var req updateTeamRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "update-team", id, ws, "failed", "invalid_body")
		return
	}
	if req.Name == nil && req.DepartmentID == nil {
		writeJSONError(w, http.StatusBadRequest, "at least one field is required")
		h.record(r, claims, "update-team", id, ws, "failed", "empty_update")
		return
	}
	var name string
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeJSONError(w, http.StatusBadRequest, "name must not be blank")
			h.record(r, claims, "update-team", id, ws, "failed", "name_blank")
			return
		}
		if utf8.RuneCountInString(trimmed) > teamNameMaxRunes {
			writeJSONError(w, http.StatusBadRequest, "name is too long")
			h.record(r, claims, "update-team", id, ws, "failed", "name_too_long")
			return
		}
		name = trimmed
	}
	var deptID *uuid.UUID
	if req.DepartmentID != nil {
		parsed, err := uuid.Parse(strings.TrimSpace(*req.DepartmentID))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "department_id must be a UUID")
			h.record(r, claims, "update-team", id, ws, "failed", "invalid_department_id")
			return
		}
		deptID = &parsed
	}
	t, err := h.svc.Update(r.Context(), ws, id, name, deptID)
	if err != nil {
		h.fail(w, r, claims, "update-team", id, err)
		return
	}
	h.record(r, claims, "update-team", t.ID, ws, "success", "")
	writeJSON(w, http.StatusOK, toTeamResponse(t))
}

func (h *TeamHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "delete-team")
	if !ok {
		return
	}
	id, ok := teamIDFromPath(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), ws, id); err != nil {
		h.fail(w, r, claims, "delete-team", id, err)
		return
	}
	h.record(r, claims, "delete-team", id, ws, "success", "")
	w.WriteHeader(http.StatusNoContent)
}

func parseTeamQuery(w http.ResponseWriter, r *http.Request) (team.ListQuery, bool) {
	var q team.ListQuery
	known := map[string]bool{"limit": true, "cursor": true, "department_id": true}
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
		c, err := team.DecodeCursor(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "cursor is not valid")
			return q, false
		}
		q.Cursor = c
	}
	if v := values.Get("department_id"); v != "" {
		id, err := uuid.Parse(strings.TrimSpace(v))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "department_id must be a UUID")
			return q, false
		}
		q.DepartmentID = &id
	}
	q.Normalize()
	return q, true
}

func teamIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid team id")
		return uuid.Nil, false
	}
	return id, true
}

func toTeamResponse(t *team.Team) teamResponse {
	return teamResponse{
		ID:           t.ID.String(),
		DepartmentID: t.DepartmentID.String(),
		WorkspaceID:  t.WorkspaceID.String(),
		Name:         t.Name,
		CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func toTeamPage(p team.Page) teamPage {
	page := teamPage{Limit: p.Limit, NextCursor: p.NextCursor}
	page.Teams = make([]teamResponse, 0, len(p.Teams))
	for _, t := range p.Teams {
		page.Teams = append(page.Teams, toTeamResponse(t))
	}
	return page
}

var _ TeamService = (*team.Service)(nil)
