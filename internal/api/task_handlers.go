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
	"austro-os/internal/task"

	"github.com/google/uuid"
)

// TaskHandler exposes the task contract finalized in ADR-006:
//
//	POST /tasks                    create a task in the caller's workspace
//	GET  /tasks                    bounded, newest-first listing
//	GET  /tasks/{id}               read one task
//	PATCH /tasks/{id}              update permitted non-lifecycle fields
//	POST /tasks/{id}/transition    apply one lifecycle transition
//
// Two properties define the security model here.
//
// First, the workspace is never taken from the request. It comes from
// WorkspaceFromClaims, which reads the verified token, so the store binds
// app.current_workspace to the caller's own tenant and RLS does the confining.
// These paths carry no workspace segment to forge; the {id} they do carry is
// the task, and the store resolves it only within the caller's workspace, so a
// task id from another tenant is simply not found.
//
// Second, the lifecycle is server-side. A transition request names only its
// destination; the current status is read from the database inside the same
// operation and task.CanTransition decides. A client cannot assert a state, and
// cannot reach a status the lifecycle does not define.
type TaskHandler struct {
	svc TaskService
	// auditSink persists task decisions. Task lifecycle changes are the record
	// of who moved work and when, so like the auth and workspace handlers this
	// fails closed when the sink is absent.
	auditSink audit.Sink
}

// TaskService is the slice of the task domain the HTTP surface depends on. It is
// declared here rather than taken as the concrete *task.Service so the handlers
// can be exercised against a substitute, and so the API cannot reach a domain
// method it has no business calling.
type TaskService interface {
	Create(ctx context.Context, workspaceID uuid.UUID, title string, priority task.Priority, description string) (*task.Task, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*task.Task, error)
	ListPage(ctx context.Context, workspaceID uuid.UUID, q task.ListQuery) (task.Page, error)
	Update(ctx context.Context, workspaceID, id uuid.UUID, title string, priority task.Priority, description *string, assigneeType task.AssigneeType, assigneeID *uuid.UUID, deadline *time.Time) (*task.Task, error)
	Transition(ctx context.Context, workspaceID, id uuid.UUID, to task.Status) (*task.Task, error)
}

// NewTaskHandler builds the handler. The service is required; a nil service
// would turn every request into a nil dereference at request time rather than a
// startup failure.
func NewTaskHandler(svc TaskService) *TaskHandler {
	return &TaskHandler{svc: svc}
}

// SetAuditSink attaches the persistent audit writer, mirroring the auth and
// workspace handlers.
func (h *TaskHandler) SetAuditSink(s audit.Sink) *TaskHandler {
	h.auditSink = s
	return h
}

// Bounds on task input. They are stated in runes because titles and
// descriptions are user-facing text, and a byte limit would cut a multi-byte
// character in half.
const (
	taskTitleMaxRunes       = 200
	taskDescriptionMaxRunes = 4000
)

// createTaskRequest is the POST body. Priority is optional and defaults to
// normal; there is no status field, because a new task is always backlog and
// letting the caller choose would bypass the lifecycle.
type createTaskRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
}

// updateTaskRequest is the PATCH body. Every field is a pointer so that
// "absent" is distinguishable from "set to the zero value": a PATCH that omits
// the title must leave the title alone, and a request that clears the
// description must be able to say so.
type updateTaskRequest struct {
	Title        *string `json:"title"`
	Description  *string `json:"description"`
	Priority     *string `json:"priority"`
	AssigneeType *string `json:"assignee_type"`
	AssigneeID   *string `json:"assignee_id"`
	Deadline     *string `json:"deadline"`
}

// transitionTaskRequest names only the destination. The origin is read from the
// database, so the client cannot claim a starting state the task is not in.
type transitionTaskRequest struct {
	Status string `json:"status"`
}

type taskResponse struct {
	ID           string   `json:"id"`
	WorkspaceID  string   `json:"workspace_id"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Status       string   `json:"status"`
	Priority     string   `json:"priority"`
	AssigneeType string   `json:"assignee_type,omitempty"`
	AssigneeID   string   `json:"assignee_id,omitempty"`
	Deadline     string   `json:"deadline,omitempty"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Transitions  []string `json:"transitions"`
}

// taskPage is the listing envelope. next_cursor is omitted on the last page.
type taskPage struct {
	Tasks      []taskResponse `json:"tasks"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Limit      int            `json:"limit"`
}

// List returns one bounded page of the caller's tasks.
func (h *TaskHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "list-tasks")
	if !ok {
		return
	}
	q, ok := parseTaskQuery(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListPage(r.Context(), ws, q)
	if err != nil {
		h.fail(w, r, claims, "list-tasks", uuid.Nil, err)
		return
	}
	writeJSON(w, http.StatusOK, toTaskPage(page))
}

// Get returns one task.
func (h *TaskHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "get-task")
	if !ok {
		return
	}
	id, ok := taskIDFromPath(w, r)
	if !ok {
		return
	}
	t, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		h.fail(w, r, claims, "get-task", id, err)
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(t))
}

// Create adds a task to the caller's workspace. The new task is always backlog.
func (h *TaskHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "create-task")
	if !ok {
		return
	}
	var req createTaskRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "create-task", uuid.Nil, ws, "failed", "invalid_body")
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeJSONError(w, http.StatusBadRequest, "title is required")
		h.record(r, claims, "create-task", uuid.Nil, ws, "failed", "title_required")
		return
	}
	if utf8.RuneCountInString(title) > taskTitleMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "title exceeds the maximum length")
		h.record(r, claims, "create-task", uuid.Nil, ws, "failed", "title_too_long")
		return
	}
	if utf8.RuneCountInString(req.Description) > taskDescriptionMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "description exceeds the maximum length")
		h.record(r, claims, "create-task", uuid.Nil, ws, "failed", "description_too_long")
		return
	}
	priority, ok := optionalPriority(w, req.Priority)
	if !ok {
		h.record(r, claims, "create-task", uuid.Nil, ws, "failed", "invalid_priority")
		return
	}

	t, err := h.svc.Create(r.Context(), ws, title, priority, strings.TrimSpace(req.Description))
	if err != nil {
		h.fail(w, r, claims, "create-task", uuid.Nil, err)
		return
	}
	h.record(r, claims, "create-task", t.ID, ws, "success", "")
	writeJSON(w, http.StatusCreated, toTaskResponse(t))
}

// Update applies non-lifecycle field changes. Status is not among them: it moves
// only through Transition, so a PATCH cannot smuggle a lifecycle change past the
// validation there.
func (h *TaskHandler) Update(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "update-task")
	if !ok {
		return
	}
	id, ok := taskIDFromPath(w, r)
	if !ok {
		return
	}
	var req updateTaskRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "update-task", id, ws, "failed", "invalid_body")
		return
	}

	title := ""
	if req.Title != nil {
		title = strings.TrimSpace(*req.Title)
		if title == "" {
			writeJSONError(w, http.StatusBadRequest, "title must not be blank")
			h.record(r, claims, "update-task", id, ws, "failed", "title_blank")
			return
		}
		if utf8.RuneCountInString(title) > taskTitleMaxRunes {
			writeJSONError(w, http.StatusBadRequest, "title exceeds the maximum length")
			h.record(r, claims, "update-task", id, ws, "failed", "title_too_long")
			return
		}
	}
	var description *string
	if req.Description != nil {
		if utf8.RuneCountInString(*req.Description) > taskDescriptionMaxRunes {
			writeJSONError(w, http.StatusBadRequest, "description exceeds the maximum length")
			h.record(r, claims, "update-task", id, ws, "failed", "description_too_long")
			return
		}
		trimmed := strings.TrimSpace(*req.Description)
		description = &trimmed
	}
	priority := task.Priority("")
	if req.Priority != nil {
		// On update an explicit-but-empty priority is refused rather than
		// defaulted. Defaulting here would silently reset the priority of any
		// task patched with "priority": "", and an absent field -- not an empty
		// one -- is how a caller says "leave it alone".
		if strings.TrimSpace(*req.Priority) == "" {
			writeJSONError(w, http.StatusBadRequest, "priority must not be blank")
			h.record(r, claims, "update-task", id, ws, "failed", "priority_blank")
			return
		}
		p, ok := optionalPriority(w, *req.Priority)
		if !ok {
			h.record(r, claims, "update-task", id, ws, "failed", "invalid_priority")
			return
		}
		priority = p
	}
	assigneeType := task.AssigneeType("")
	if req.AssigneeType != nil {
		at := task.AssigneeType(strings.TrimSpace(*req.AssigneeType))
		if !task.ValidAssigneeType(at) {
			writeJSONError(w, http.StatusBadRequest, "assignee_type must be ai_employee or human")
			h.record(r, claims, "update-task", id, ws, "failed", "invalid_assignee_type")
			return
		}
		assigneeType = at
	}
	var assigneeID *uuid.UUID
	if req.AssigneeID != nil {
		parsed, err := uuid.Parse(strings.TrimSpace(*req.AssigneeID))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "assignee_id must be a UUID")
			h.record(r, claims, "update-task", id, ws, "failed", "invalid_assignee_id")
			return
		}
		assigneeID = &parsed
	}
	var deadline *time.Time
	if req.Deadline != nil {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.Deadline))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "deadline must be an RFC 3339 timestamp")
			h.record(r, claims, "update-task", id, ws, "failed", "invalid_deadline")
			return
		}
		utc := parsed.UTC()
		deadline = &utc
	}

	t, err := h.svc.Update(r.Context(), ws, id, title, priority, description, assigneeType, assigneeID, deadline)
	if err != nil {
		h.fail(w, r, claims, "update-task", id, err)
		return
	}
	h.record(r, claims, "update-task", t.ID, ws, "success", "")
	writeJSON(w, http.StatusOK, toTaskResponse(t))
}

// Transition applies one lifecycle move. The destination is validated against
// the task's current status, which is read inside the service, so the client
// cannot assert where the task is now.
func (h *TaskHandler) Transition(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "transition-task")
	if !ok {
		return
	}
	id, ok := taskIDFromPath(w, r)
	if !ok {
		return
	}
	var req transitionTaskRequest
	if !decodeJSON(w, r, &req) {
		h.record(r, claims, "transition-task", id, ws, "failed", "invalid_body")
		return
	}
	to := task.Status(strings.TrimSpace(req.Status))
	if to == "" {
		writeJSONError(w, http.StatusBadRequest, "status is required")
		h.record(r, claims, "transition-task", id, ws, "failed", "status_required")
		return
	}
	if !task.ValidStatus(to) {
		writeJSONError(w, http.StatusBadRequest, "status is not a task lifecycle stage")
		h.record(r, claims, "transition-task", id, ws, "failed", "unknown_status")
		return
	}

	t, err := h.svc.Transition(r.Context(), ws, id, to)
	if err != nil {
		h.fail(w, r, claims, "transition-task", id, err)
		return
	}
	h.record(r, claims, "transition-task", t.ID, ws, "success", string(to))
	writeJSON(w, http.StatusOK, toTaskResponse(t))
}

// scope resolves the authenticated caller and the workspace every task
// operation is confined to. The workspace comes from the verified claims and
// from nothing else; a caller with no workspace -- which includes the founder,
// who has none by database constraint -- is refused.
func (h *TaskHandler) scope(w http.ResponseWriter, r *http.Request, action string) (*auth.Claims, uuid.UUID, bool) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, uuid.Nil, false
	}
	binding, err := WorkspaceFromClaims(claims)
	if err != nil {
		auditWorkspace(claims.ID, action, "denied", "no_workspace_context")
		h.record(r, claims, action, uuid.Nil, uuid.Nil, "denied", "no_workspace_context")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	ws, err := uuid.Parse(binding)
	if err != nil {
		auditWorkspace(claims.ID, action, "denied", "invalid_workspace_claim")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	return claims, ws, true
}

// fail classifies a domain error and records it. Client mistakes are 4xx; only
// something the server got wrong is a 500. Getting this the wrong way round
// tells an operator to investigate input validation as though it were an
// outage.
func (h *TaskHandler) fail(w http.ResponseWriter, r *http.Request, claims *auth.Claims, action string, id uuid.UUID, err error) {
	switch {
	case errors.Is(err, task.ErrNotFound):
		h.record(r, claims, action, id, uuid.Nil, "failed", "not_found")
		writeJSONError(w, http.StatusNotFound, "task not found")
	case errors.Is(err, task.ErrWorkspaceMismatch):
		h.record(r, claims, action, id, uuid.Nil, "denied", "workspace_mismatch")
		writeJSONError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, task.ErrInvalidTransition),
		errors.Is(err, task.ErrTerminalState),
		errors.Is(err, task.ErrStatus),
		errors.Is(err, task.ErrPriority),
		errors.Is(err, task.ErrAssigneeType),
		errors.Is(err, task.ErrInvalidInput),
		errors.Is(err, task.ErrInvalidCursor):
		h.record(r, claims, action, id, uuid.Nil, "failed", "invalid_request")
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		logger.NewEntry("task-handler-error").SetLevel("error").
			With("action", action).WithError(err).Log()
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// record persists a task decision to the audit chain. A task lifecycle change is
// the evidence of who moved work, so it is written durably rather than only
// logged.
func (h *TaskHandler) record(r *http.Request, claims *auth.Claims, action string, taskID, ws uuid.UUID, outcome, detail string) {
	if h.auditSink == nil {
		return
	}
	rec := audit.Record{
		// The action names read "create-task", "transition-task" and so on; the
		// audit event type is the verb alone, so "task.create" rather than
		// "task.create-task". Deriving it here keeps the two spellings from
		// drifting apart across call sites.
		EventType:  "task." + strings.TrimSuffix(action, "-task"),
		ActorType:  "user",
		ActorID:    parseActorID(claims.ID),
		TargetType: "task",
		TargetID:   taskID,
		Outcome:    outcome,
		Principle:  "Human Oversight",
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
		logger.NewEntry("task-audit-persist-failed").SetLevel("error").
			With("event_type", rec.EventType).WithError(err).Log()
	}
}

// parseTaskQuery reads the bounded listing parameters. Unknown parameters are
// refused rather than ignored, so a misspelled filter cannot silently return an
// unfiltered page that looks like it worked.
func parseTaskQuery(w http.ResponseWriter, r *http.Request) (task.ListQuery, bool) {
	var q task.ListQuery
	known := map[string]bool{"status": true, "limit": true, "cursor": true}
	// Parsed explicitly rather than through r.URL.Query(). That method swallows
	// the parse error and returns whatever it managed to read, and since Go 1.17
	// url.ParseQuery rejects ";" as a separator -- so a query string containing
	// one had the offending pair silently dropped and the request served as
	// though the parameter had never been sent. A caller whose cursor was
	// mangled would get the first page back and believe it had paged forward.
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
	if v := strings.TrimSpace(values.Get("status")); v != "" {
		st, err := task.ValidateStatus(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "status is not a task lifecycle stage")
			return q, false
		}
		q.Status = &st
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
		c, err := task.DecodeCursor(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "cursor is not valid")
			return q, false
		}
		q.Before = c
	}
	// Applied here as well as in the service so the response reports the page
	// size actually used rather than the one that was asked for.
	q.Normalize()
	return q, true
}

// taskIDFromPath reads the task identifier. A malformed one is a client error.
func taskIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid task id")
		return uuid.Nil, false
	}
	return id, true
}

// optionalPriority validates a priority that may be omitted.
//
// An omitted priority becomes normal rather than being passed through empty. The
// domain requires a valid priority on create, so forwarding "" would turn an
// optional field into a mandatory one and contradict both the OpenAPI contract
// and the column default. On update an empty value still means "leave it
// alone", which is what the service does with it.
func optionalPriority(w http.ResponseWriter, raw string) (task.Priority, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return task.PriorityNormal, true
	}
	p := task.Priority(trimmed)
	if !task.ValidPriority(p) {
		writeJSONError(w, http.StatusBadRequest, "priority must be low, normal, high or urgent")
		return task.Priority(""), false
	}
	return p, true
}

func toTaskResponse(t *task.Task) taskResponse {
	resp := taskResponse{
		ID:           t.ID.String(),
		WorkspaceID:  t.WorkspaceID.String(),
		Title:        t.Title,
		Description:  t.Description,
		Status:       string(t.Status),
		Priority:     string(t.Priority),
		AssigneeType: string(t.AssigneeType),
		CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:    t.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if t.AssigneeID != nil {
		resp.AssigneeID = t.AssigneeID.String()
	}
	if t.Deadline != nil {
		resp.Deadline = t.Deadline.UTC().Format(time.RFC3339)
	}
	// The permitted next states are computed server-side from the same table the
	// service enforces, so a client cannot offer a transition the server would
	// refuse. This is a convenience derived from the authority, never the
	// authority itself.
	for _, candidate := range []task.Status{
		task.StatusBacklog, task.StatusPlanned, task.StatusInProgress, task.StatusInReview,
		task.StatusCompleted, task.StatusCancelled, task.StatusRejected, task.StatusFailed,
	} {
		if task.CanTransition(t.Status, candidate) == nil {
			resp.Transitions = append(resp.Transitions, string(candidate))
		}
	}
	if resp.Transitions == nil {
		resp.Transitions = []string{}
	}
	return resp
}

func toTaskPage(p task.Page) taskPage {
	page := taskPage{Limit: p.Limit, NextCursor: p.NextCursor}
	page.Tasks = make([]taskResponse, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		page.Tasks = append(page.Tasks, toTaskResponse(t))
	}
	return page
}

// compile-time proof that the production service satisfies the port the handler
// depends on.
var _ TaskService = (*task.Service)(nil)
