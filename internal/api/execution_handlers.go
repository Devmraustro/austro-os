package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"austro-os/internal/aiemployee"
	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/execution"
	logger "austro-os/internal/log"
	"austro-os/internal/middleware"
	"austro-os/internal/task"

	"github.com/google/uuid"
)

// ExecutionHandler serves the AI execution surface: admission on a task, one
// execution read, and the named human cancel command. It composes the execution
// service with the task and employee services so the admission snapshot is
// built only from data the caller can already see inside its own workspace.
type ExecutionHandler struct {
	svc       ExecutionService
	tasks     TaskService
	employees AIEmployeeService
	// auditSink persists execution decisions. Admission and cancellation move
	// work, so like the task handler this fails closed when the sink is absent.
	auditSink audit.Sink
}

// ExecutionService is the slice of the execution domain the HTTP surface
// depends on, declared here so the handlers run against a substitute in tests.
type ExecutionService interface {
	Admit(ctx context.Context, req execution.AdmitRequest) (execution.AdmitResult, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*execution.Execution, error)
	Cancel(ctx context.Context, workspaceID, id uuid.UUID) error
}

// NewExecutionHandler builds the handler. The service is required; a nil
// service would turn every request into a nil dereference at request time.
func NewExecutionHandler(svc ExecutionService, tasks TaskService, employees AIEmployeeService) *ExecutionHandler {
	return &ExecutionHandler{svc: svc, tasks: tasks, employees: employees}
}

// SetAuditSink attaches the persistent audit writer, mirroring the other
// mutation handlers.
func (h *ExecutionHandler) SetAuditSink(s audit.Sink) *ExecutionHandler {
	h.auditSink = s
	return h
}

// executeTaskRequest is the POST /tasks/{id}/execute body. ai_employee_id names
// which AI employee runs the task; max_attempts is optional (the domain applies
// its default); idempotency_key, when present, makes admission replay-safe.
type executeTaskRequest struct {
	AIEmployeeID   string `json:"ai_employee_id"`
	MaxAttempts    int    `json:"max_attempts"`
	IdempotencyKey string `json:"idempotency_key"`
}

// executionResponse is the wire shape of an execution. result_json carries the
// persisted provider output verbatim when the execution finished.
type executionResponse struct {
	ID             string          `json:"id"`
	WorkspaceID    string          `json:"workspace_id"`
	AIEmployeeID   string          `json:"ai_employee_id"`
	TaskID         string          `json:"task_id,omitempty"`
	Status         string          `json:"status"`
	Attempt        int             `json:"attempt"`
	MaxAttempts    int             `json:"max_attempts"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	LastErrorCode  *string         `json:"last_error_code,omitempty"`
	ErrorDetail    *string         `json:"error_detail,omitempty"`
	ResultJSON     json.RawMessage `json:"result_json,omitempty"`
	Created        bool            `json:"created"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	CompletedAt    string          `json:"completed_at,omitempty"`
	FailedAt       string          `json:"failed_at,omitempty"`
	CancelledAt    string          `json:"cancelled_at,omitempty"`
}

// Execute admits a task to the runtime. A fresh admission is 202 with
// created=true; a completed idempotent replay is 200 with the result; a
// conflicting key is 409 and an over-capacity employee is 429.
func (h *ExecutionHandler) Execute(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "execute-task")
	if !ok {
		return
	}
	taskID, ok := taskIDFromPath(w, r)
	if !ok {
		return
	}
	var body executeTaskRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	employeeID, err := uuid.Parse(strings.TrimSpace(body.AIEmployeeID))
	if err != nil || employeeID == uuid.Nil {
		writeJSONError(w, http.StatusBadRequest, "ai_employee_id must be a uuid")
		return
	}

	// The snapshot captures only what the caller can already see: task and
	// employee are resolved inside the caller's workspace, never by ids the
	// request carries.
	t, err := h.tasks.Get(r.Context(), ws, taskID)
	if err != nil {
		h.fail(w, r, claims, "execute-task", taskID, err)
		return
	}
	emp, err := h.employees.Get(r.Context(), ws, employeeID)
	if err != nil {
		h.fail(w, r, claims, "execute-task", taskID, err)
		return
	}

	res, err := h.svc.Admit(r.Context(), execution.AdmitRequest{
		WorkspaceID:    ws,
		AIEmployeeID:   employeeID,
		TaskID:         &taskID,
		MaxAttempts:    body.MaxAttempts,
		IdempotencyKey: strings.TrimSpace(body.IdempotencyKey),
		Context: &execution.MaterializedRequest{
			TaskTitle:       t.Title,
			TaskDescription: t.Description,
			AIEmployeeName:  emp.Name,
			AIEmployeeRole:  emp.Role,
			Capabilities:    emp.Capabilities,
		},
	})
	if err != nil {
		h.fail(w, r, claims, "execute-task", taskID, err)
		return
	}

	status := http.StatusAccepted
	if res.Completed {
		status = http.StatusOK
	}
	target := uuid.Nil
	if res.Execution != nil {
		target = res.Execution.ID
	}
	if !h.recordSuccess(w, r, claims, "execute-task", target, ws, "") {
		return
	}
	writeJSON(w, status, toExecutionResponse(res.Execution, res.Created))
}

// Get returns one execution inside the caller's workspace.
func (h *ExecutionHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "get-execution")
	if !ok {
		return
	}
	id, ok := executionIDFromPath(w, r)
	if !ok {
		return
	}
	exec, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		h.fail(w, r, claims, "get-execution", id, err)
		return
	}
	writeJSON(w, http.StatusOK, toExecutionResponse(exec, false))
}

// Cancel aborts a pending/running execution. It is idempotent: an execution
// already terminal yields the terminal row rather than an error.
func (h *ExecutionHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "cancel-execution")
	if !ok {
		return
	}
	id, ok := executionIDFromPath(w, r)
	if !ok {
		return
	}
	if err := h.svc.Cancel(r.Context(), ws, id); err != nil {
		h.fail(w, r, claims, "cancel-execution", id, err)
		return
	}
	exec, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		h.fail(w, r, claims, "cancel-execution", id, err)
		return
	}
	if !h.recordSuccess(w, r, claims, "cancel-execution", id, ws, "") {
		return
	}
	writeJSON(w, http.StatusAccepted, toExecutionResponse(exec, false))
}

// scope resolves the caller's workspace from the verified claims. A caller
// with no workspace — which includes the founder, who has none by database
// constraint — is refused, mirroring the task handler.
func (h *ExecutionHandler) scope(w http.ResponseWriter, r *http.Request, action string) (*auth.Claims, uuid.UUID, bool) {
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
		h.record(r, claims, action, uuid.Nil, uuid.Nil, "denied", "invalid_workspace_claim")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	return claims, ws, true
}

// fail classifies a domain error across the task, employee and execution
// surfaces and records it. Client mistakes are 4xx; only something the server
// got wrong is a 500.
func (h *ExecutionHandler) fail(w http.ResponseWriter, r *http.Request, claims *auth.Claims, action string, id uuid.UUID, err error) {
	switch {
	case errors.Is(err, task.ErrNotFound),
		errors.Is(err, aiemployee.ErrNotFound),
		errors.Is(err, execution.ErrNotFound):
		h.record(r, claims, action, id, uuid.Nil, "failed", "not_found")
		writeJSONError(w, http.StatusNotFound, "not found")
	case errors.Is(err, execution.ErrCapacityExceeded):
		h.record(r, claims, action, id, uuid.Nil, "failed", "capacity_exceeded")
		writeJSONError(w, http.StatusTooManyRequests, "ai employee concurrency capacity exceeded")
	case errors.Is(err, execution.ErrIdempotencyKey):
		h.record(r, claims, action, id, uuid.Nil, "failed", "idempotency_key_conflict")
		writeJSONError(w, http.StatusConflict, "idempotency key conflict")
	case errors.Is(err, task.ErrWorkspaceMismatch),
		errors.Is(err, aiemployee.ErrWorkspaceMismatch),
		errors.Is(err, execution.ErrWorkspaceMismatch):
		h.record(r, claims, action, id, uuid.Nil, "denied", "workspace_mismatch")
		writeJSONError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, task.ErrInvalidInput),
		errors.Is(err, task.ErrInvalidTransition),
		errors.Is(err, task.ErrTerminalState),
		errors.Is(err, task.ErrStatus),
		errors.Is(err, task.ErrPriority),
		errors.Is(err, task.ErrAssigneeType),
		errors.Is(err, aiemployee.ErrInvalidInput),
		errors.Is(err, execution.ErrInvalidInput),
		errors.Is(err, execution.ErrStatus):
		h.record(r, claims, action, id, uuid.Nil, "failed", "invalid_request")
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		logger.NewEntry("execution-handler-error").SetLevel("error").
			With("action", action).WithError(err).Log()
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// appendAudit writes one execution decision to the audit chain and returns any
// failure, mirroring the task handler's fail-closed convention.
func (h *ExecutionHandler) appendAudit(r *http.Request, claims *auth.Claims, action string, targetID, ws uuid.UUID, outcome, detail string) error {
	if h.auditSink == nil {
		return errAuditUnavailable
	}
	rec := audit.Record{
		EventType:  "execution." + strings.TrimSuffix(action, "-execution"),
		ActorType:  "user",
		ActorID:    parseActorID(claims.ID),
		TargetType: "execution",
		TargetID:   targetID,
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
		logger.NewEntry("execution-audit-persist-failed").SetLevel("error").
			With("event_type", rec.EventType).WithError(err).Log()
		return err
	}
	return nil
}

// record persists a denial or failure best-effort.
func (h *ExecutionHandler) record(r *http.Request, claims *auth.Claims, action string, targetID, ws uuid.UUID, outcome, detail string) {
	_ = h.appendAudit(r, claims, action, targetID, ws, outcome, detail)
}

// recordSuccess persists a successful mutation and fails the request when the
// record could not be written, so admission and cancellation never answer 2xx
// with a missing audit entry.
func (h *ExecutionHandler) recordSuccess(w http.ResponseWriter, r *http.Request, claims *auth.Claims, action string, targetID, ws uuid.UUID, detail string) bool {
	if err := h.appendAudit(r, claims, action, targetID, ws, "success", detail); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "audit unavailable")
		return false
	}
	return true
}

// executionIDFromPath reads the execution identifier.
func executionIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid execution id")
		return uuid.Nil, false
	}
	return id, true
}

// toExecutionResponse maps the aggregate to its wire shape.
func toExecutionResponse(e *execution.Execution, created bool) executionResponse {
	if e == nil {
		return executionResponse{Created: created}
	}
	resp := executionResponse{
		ID:           e.ID.String(),
		WorkspaceID:  e.WorkspaceID.String(),
		AIEmployeeID: e.AIEmployeeID.String(),
		Status:       string(e.Status),
		Attempt:      e.Attempt,
		MaxAttempts:  e.MaxAttempts,
		Created:      created,
		CreatedAt:    e.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:    e.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if e.TaskID != nil {
		resp.TaskID = e.TaskID.String()
	}
	if e.IdempotencyKey != nil {
		resp.IdempotencyKey = *e.IdempotencyKey
	}
	if e.LastErrorCode != execution.CodeNone {
		code := e.LastErrorCode.String()
		resp.LastErrorCode = &code
	}
	if e.ErrorDetail != nil {
		resp.ErrorDetail = e.ErrorDetail
	}
	if len(e.ResultJSON) > 0 {
		resp.ResultJSON = json.RawMessage(e.ResultJSON)
	}
	if e.CompletedAt != nil {
		resp.CompletedAt = e.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	if e.FailedAt != nil {
		resp.FailedAt = e.FailedAt.UTC().Format(time.RFC3339Nano)
	}
	if e.CancelledAt != nil {
		resp.CancelledAt = e.CancelledAt.UTC().Format(time.RFC3339Nano)
	}
	return resp
}
