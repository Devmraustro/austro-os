package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"austro-os/internal/auth"
	"austro-os/internal/middleware"
	"austro-os/internal/orchestration"
	"austro-os/internal/rbac"
	"github.com/google/uuid"
)

// PipelineHandler exposes only named Creator operations. There is intentionally
// no PATCH, arbitrary stage endpoint, cancel command, or client-controlled
// worker status.
type PipelineHandler struct{ svc PipelineService }

type PipelineService interface {
	CreateWithIdempotency(context.Context, uuid.UUID, *uuid.UUID, string, string) (*orchestration.Pipeline, error)
	Get(context.Context, uuid.UUID, uuid.UUID) (*orchestration.Pipeline, error)
	List(context.Context, uuid.UUID) ([]*orchestration.Pipeline, error)
	Approve(context.Context, uuid.UUID, uuid.UUID, string) (*orchestration.Pipeline, error)
	Retry(context.Context, uuid.UUID, uuid.UUID, string) (*orchestration.Pipeline, error)
}

func NewPipelineHandler(svc PipelineService) *PipelineHandler { return &PipelineHandler{svc: svc} }

type createPipelineRequest struct {
	GoalID string `json:"goal_id"`
}
type pipelineListResponse struct {
	Pipelines []*orchestration.Pipeline `json:"pipelines"`
	Count     int                       `json:"count"`
	Limit     int                       `json:"limit"`
}

const pipelinePageSize = 100

func (h *PipelineHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req createPipelineRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var goalID *uuid.UUID
	if strings.TrimSpace(req.GoalID) != "" {
		id, err := uuid.Parse(req.GoalID)
		if err != nil || id == uuid.Nil {
			writeJSONError(w, http.StatusBadRequest, "invalid goal_id")
			return
		}
		goalID = &id
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 128 {
		writeJSONError(w, http.StatusBadRequest, "idempotency key is too long")
		return
	}
	p, err := h.svc.CreateWithIdempotency(pipelineContext(r, claims), ws, goalID, traceFromRequest(r), key)
	if err != nil {
		h.pipelineError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *PipelineHandler) List(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := h.scope(w, r)
	if !ok {
		return
	}
	limit, valid := pipelineLimit(r)
	if !valid {
		writeJSONError(w, http.StatusBadRequest, "invalid pipeline list query")
		return
	}
	items, err := h.svc.List(r.Context(), ws)
	if err != nil {
		h.pipelineError(w, err)
		return
	}
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, pipelineListResponse{Pipelines: items, Count: len(items), Limit: limit})
}

func (h *PipelineHandler) Get(w http.ResponseWriter, r *http.Request) {
	_, ws, ok := h.scope(w, r)
	if !ok {
		return
	}
	id, ok := pipelineID(w, r)
	if !ok {
		return
	}
	p, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		h.pipelineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *PipelineHandler) Approve(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r)
	if !ok {
		return
	}
	if claims.Role != rbac.RoleWorkspaceAdmin {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !emptyBody(w, r) {
		return
	}
	id, ok := pipelineID(w, r)
	if !ok {
		return
	}
	p, err := h.svc.Approve(pipelineContext(r, claims), ws, id, claims.ID)
	if err != nil {
		h.pipelineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *PipelineHandler) Retry(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r)
	if !ok {
		return
	}
	if !emptyBody(w, r) {
		return
	}
	id, ok := pipelineID(w, r)
	if !ok {
		return
	}
	p, err := h.svc.Retry(pipelineContext(r, claims), ws, id, claims.ID)
	if err != nil {
		h.pipelineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *PipelineHandler) scope(w http.ResponseWriter, r *http.Request) (*auth.Claims, uuid.UUID, bool) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, uuid.Nil, false
	}
	if !rbac.ValidRole(claims.Role) || strings.TrimSpace(claims.ID) == "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	if _, err := uuid.Parse(claims.ID); err != nil {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	binding, err := WorkspaceFromClaims(claims)
	if err != nil {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	ws, err := uuid.Parse(binding)
	if err != nil || ws == uuid.Nil {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	return claims, ws, true
}

func pipelineContext(r *http.Request, claims *auth.Claims) context.Context {
	cv := middleware.ExtractContextValues(r)
	return orchestration.WithActor(orchestration.WithTrace(r.Context(), cv.TraceID, cv.SpanID), claims.ID)
}
func traceFromRequest(r *http.Request) string { return middleware.ExtractContextValues(r).TraceID }

func pipelineID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || id == uuid.Nil {
		writeJSONError(w, http.StatusBadRequest, "invalid pipeline id")
		return uuid.Nil, false
	}
	return id, true
}

func pipelineLimit(r *http.Request) (int, bool) {
	q := r.URL.Query()
	for key := range q {
		if key != "limit" {
			return 0, false
		}
	}
	if len(q["limit"]) > 1 {
		return 0, false
	}
	if len(q["limit"]) == 0 || q.Get("limit") == "" {
		return pipelinePageSize, true
	}
	n, err := strconv.Atoi(q.Get("limit"))
	if err != nil || n < 1 || n > pipelinePageSize {
		return 0, false
	}
	return n, true
}

func (h *PipelineHandler) pipelineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orchestration.ErrNotFound), errors.Is(err, orchestration.ErrWorkspaceMismatch):
		writeJSONError(w, http.StatusNotFound, "pipeline not found")
	case errors.Is(err, orchestration.ErrInvalidInput), errors.Is(err, orchestration.ErrInvalidStage), errors.Is(err, orchestration.ErrInvalidStatus):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, orchestration.ErrApprovalRequired), errors.Is(err, orchestration.ErrInvalidTransition), errors.Is(err, orchestration.ErrTerminalState), errors.Is(err, orchestration.ErrConsecutiveAdvance):
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, orchestration.ErrConcurrentUpdate):
		writeJSONError(w, http.StatusConflict, "pipeline changed; retry the request")
	case errors.Is(err, orchestration.ErrRateLimited):
		writeJSONError(w, http.StatusTooManyRequests, "pipeline rate limit exceeded")
	case errors.Is(err, orchestration.ErrUnauthorizedActor):
		writeJSONError(w, http.StatusForbidden, "forbidden")
	default:
		writeJSONError(w, http.StatusBadGateway, "pipeline stage failed")
	}
}
