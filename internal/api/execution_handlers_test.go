package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"austro-os/internal/aiemployee"
	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/execution"
	"austro-os/internal/rbac"
	"austro-os/internal/task"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakeExecutionService is an in-memory stand-in for ExecutionService. The
// handler tests focus on the HTTP contract (statuses, error mapping, audit), so
// the admission semantics themselves are exercised against the real execution
// service in internal/execution/service_test.go.
type fakeExecutionService struct {
	admit  func(ctx context.Context, req execution.AdmitRequest) (execution.AdmitResult, error)
	get    func(ctx context.Context, workspaceID, id uuid.UUID) (*execution.Execution, error)
	cancel func(ctx context.Context, workspaceID, id uuid.UUID) error
}

func (f *fakeExecutionService) Admit(ctx context.Context, req execution.AdmitRequest) (execution.AdmitResult, error) {
	if f.admit == nil {
		panic("fakeExecutionService.Admit not configured")
	}
	return f.admit(ctx, req)
}

func (f *fakeExecutionService) Get(ctx context.Context, workspaceID, id uuid.UUID) (*execution.Execution, error) {
	if f.get == nil {
		panic("fakeExecutionService.Get not configured")
	}
	return f.get(ctx, workspaceID, id)
}

func (f *fakeExecutionService) Cancel(ctx context.Context, workspaceID, id uuid.UUID) error {
	if f.cancel == nil {
		panic("fakeExecutionService.Cancel not configured")
	}
	return f.cancel(ctx, workspaceID, id)
}

// fakeTasks satisfies TaskService by embedding the interface and overriding the
// only method the execution handler calls.
type fakeTasks struct {
	TaskService
	tasks map[uuid.UUID]*task.Task
}

func (f *fakeTasks) Get(_ context.Context, ws, id uuid.UUID) (*task.Task, error) {
	t, ok := f.tasks[id]
	if !ok || t.WorkspaceID != ws {
		return nil, task.ErrNotFound
	}
	return t, nil
}

// fakeEmployees satisfies AIEmployeeService by embedding the interface and
// overriding the only method the execution handler calls.
type fakeEmployees struct {
	AIEmployeeService
	employees map[uuid.UUID]*aiemployee.AIEmployee
}

func (f *fakeEmployees) Get(_ context.Context, ws, id uuid.UUID) (*aiemployee.AIEmployee, error) {
	e, ok := f.employees[id]
	if !ok || e.WorkspaceID != ws {
		return nil, aiemployee.ErrNotFound
	}
	return e, nil
}

func execClaims(role rbac.Role, ws string) *auth.Claims {
	return &auth.Claims{
		ID:          uuid.NewString(),
		Role:        role,
		WorkspaceID: ws,
		Permissions: rbac.PermissionsForRole(role),
	}
}

// serveExecution exercises the execution routes through the same deny-by-default
// authorizer the server mounts, so an undeclared grant surfaces as a forbidden
// in the test just as it would over the wire.
func serveExecution(t *testing.T, svc ExecutionService, sink audit.Sink, claims *auth.Claims, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	tasks := &fakeTasks{tasks: map[uuid.UUID]*task.Task{
		uuid.MustParse(taskID): {ID: uuid.MustParse(taskID), WorkspaceID: uuid.MustParse(wsID), Title: "Ship the report", Description: "Draft it with the sources"},
	}}
	employees := &fakeEmployees{employees: map[uuid.UUID]*aiemployee.AIEmployee{
		uuid.MustParse(employeeID): {ID: uuid.MustParse(employeeID), WorkspaceID: uuid.MustParse(wsID), Name: "Aria", Role: "Researcher", Capabilities: []string{"research"}},
	}}
	h := NewExecutionHandler(svc, tasks, employees).SetAuditSink(sink)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks/{id}/execute", h.Execute)
	mux.HandleFunc("GET /executions/{id}", h.Get)
	mux.HandleFunc("POST /executions/{id}/cancel", h.Cancel)

	az := authz.NewAuthorizer()
	az.AddRules(rbac.ImplementedRules())
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := az.Authorize(r, r.Method, r.URL.Path); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})

	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if claims != nil {
		req = auth.WithClaims(req, claims)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

const (
	wsID       = "11111111-1111-1111-1111-111111111111"
	taskID     = "22222222-2222-2222-2222-222222222222"
	employeeID = "33333333-3333-3333-3333-333333333333"
	exeID      = "44444444-4444-4444-4444-444444444444"
)

func newExecution(status execution.Status) *execution.Execution {
	now := time.Now().UTC()
	return &execution.Execution{
		ID:           uuid.MustParse(exeID),
		WorkspaceID:  uuid.MustParse(wsID),
		AIEmployeeID: uuid.MustParse(employeeID),
		TaskID:       uuidPtr(taskID),
		Status:       status,
		MaxAttempts:  3,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func uuidPtr(s string) *uuid.UUID {
	u := uuid.MustParse(s)
	return &u
}

func TestExecuteFreshAdmissionIsAccepted(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, req execution.AdmitRequest) (execution.AdmitResult, error) {
		require.Equal(t, uuid.MustParse(wsID), req.WorkspaceID)
		require.Equal(t, uuid.MustParse(taskID), *req.TaskID)
		require.Equal(t, uuid.MustParse(employeeID), req.AIEmployeeID)
		require.Equal(t, "my-key", req.IdempotencyKey)
		require.NotNil(t, req.Context)
		require.Equal(t, "Ship the report", req.Context.TaskTitle)
		require.Equal(t, "Aria", req.Context.AIEmployeeName)
		return execution.AdmitResult{Execution: newExecution(execution.StatusPending), Created: true}, nil
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/tasks/"+taskID+"/execute",
		`{"ai_employee_id":"`+employeeID+`","idempotency_key":"my-key"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "pending", body["status"])
	require.Equal(t, true, body["created"])
}

func TestExecuteCompletedReplayReturns200(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, _ execution.AdmitRequest) (execution.AdmitResult, error) {
		exec := newExecution(execution.StatusCompleted)
		exec.ResultJSON = []byte(`{"draft":"done"}`)
		return execution.AdmitResult{Execution: exec, Created: false, Completed: true}, nil
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceMember, wsID),
		http.MethodPost, "/tasks/"+taskID+"/execute",
		`{"ai_employee_id":"`+employeeID+`"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "completed", body["status"])
	require.Equal(t, false, body["created"])
	raw, err := json.Marshal(body["result_json"])
	require.NoError(t, err)
	require.JSONEq(t, `{"draft":"done"}`, string(raw))
}

func TestExecuteCapacityExceededIs429(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, _ execution.AdmitRequest) (execution.AdmitResult, error) {
		return execution.AdmitResult{}, execution.ErrCapacityExceeded
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/tasks/"+taskID+"/execute", `{"ai_employee_id":"`+employeeID+`"}`)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestExecuteIdempotencyConflictIs409(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, _ execution.AdmitRequest) (execution.AdmitResult, error) {
		return execution.AdmitResult{}, execution.ErrIdempotencyKey
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/tasks/"+taskID+"/execute", `{"ai_employee_id":"`+employeeID+`"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestExecuteUnknownTaskIs404(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, _ execution.AdmitRequest) (execution.AdmitResult, error) {
		t.Fatal("admit must not run when the task is unknown")
		return execution.AdmitResult{}, nil
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/tasks/55555555-5555-5555-5555-555555555555/execute", `{"ai_employee_id":"`+employeeID+`"}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestExecuteUnknownEmployeeIs404(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, _ execution.AdmitRequest) (execution.AdmitResult, error) {
		t.Fatal("admit must not run when the employee is unknown")
		return execution.AdmitResult{}, nil
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/tasks/"+taskID+"/execute", `{"ai_employee_id":"66666666-6666-6666-6666-666666666666"}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestExecuteMissingEmployeeIDIs400(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, _ execution.AdmitRequest) (execution.AdmitResult, error) {
		t.Fatal("admit must not run without an employee id")
		return execution.AdmitResult{}, nil
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/tasks/"+taskID+"/execute", `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetExecutionReturnsIt(t *testing.T) {
	svc := &fakeExecutionService{get: func(_ context.Context, ws, id uuid.UUID) (*execution.Execution, error) {
		require.Equal(t, uuid.MustParse(wsID), ws)
		require.Equal(t, uuid.MustParse(exeID), id)
		return newExecution(execution.StatusRunning), nil
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceMember, wsID),
		http.MethodGet, "/executions/"+exeID, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "running", body["status"])
}

func TestGetUnknownExecutionIs404(t *testing.T) {
	svc := &fakeExecutionService{get: func(_ context.Context, _, _ uuid.UUID) (*execution.Execution, error) {
		return nil, execution.ErrNotFound
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodGet, "/executions/"+exeID, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCancelMarksTheExecutionCancelled(t *testing.T) {
	svc := &fakeExecutionService{
		cancel: func(_ context.Context, ws, id uuid.UUID) error {
			require.Equal(t, uuid.MustParse(wsID), ws)
			require.Equal(t, uuid.MustParse(exeID), id)
			return nil
		},
		get: func(_ context.Context, _, _ uuid.UUID) (*execution.Execution, error) {
			exec := newExecution(execution.StatusCancelled)
			now := time.Now().UTC()
			exec.CancelledAt = &now
			return exec, nil
		},
	}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/executions/"+exeID+"/cancel", "")
	require.Equal(t, http.StatusAccepted, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "cancelled", body["status"])
}

func TestCancelUnknownExecutionIs404(t *testing.T) {
	svc := &fakeExecutionService{cancel: func(_ context.Context, _, _ uuid.UUID) error {
		return execution.ErrNotFound
	}}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/executions/"+exeID+"/cancel", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestExecutionRequiresAuthentication(t *testing.T) {
	// The real router answers 401 for an anonymous caller at the RequireAuth
	// layer, which sits in front of the authorizer. Here we hit the handler
	// directly (no authz gate) so the handler's own unauthorized path is
	// exercised, mirroring handler_test's coverage of the other handlers.
	tasks := &fakeTasks{tasks: map[uuid.UUID]*task.Task{
		uuid.MustParse(taskID): {ID: uuid.MustParse(taskID), WorkspaceID: uuid.MustParse(wsID), Title: "Ship the report"},
	}}
	employees := &fakeEmployees{employees: map[uuid.UUID]*aiemployee.AIEmployee{
		uuid.MustParse(employeeID): {ID: uuid.MustParse(employeeID), WorkspaceID: uuid.MustParse(wsID), Name: "Aria", Role: "Researcher"},
	}}
	h := NewExecutionHandler(&fakeExecutionService{}, tasks, employees).SetAuditSink(&recordingSink{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /executions/{id}", h.Get)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/executions/"+exeID, nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestExecutionDeniesADenyByDefaultCaller(t *testing.T) {
	// Approved role is required, and the grant comes from the claims: a valid
	// workspace admin whose token simply carries no permission is still denied.
	claims := &auth.Claims{
		ID:          uuid.NewString(),
		Role:        rbac.RoleWorkspaceAdmin,
		WorkspaceID: wsID,
		Permissions: nil,
	}
	svc := &fakeExecutionService{}
	rec := serveExecution(t, svc, &recordingSink{}, claims,
		http.MethodPost, "/executions/"+exeID+"/cancel", "")
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestExecutionDeniesFounderWithoutWorkspace(t *testing.T) {
	// A founder has no home workspace, so ScopeSelf denies. This is the same
	// database-backed invariant the task routes enforce.
	svc := &fakeExecutionService{}
	rec := serveExecution(t, svc, &recordingSink{}, execClaims(rbac.RoleFounder, ""),
		http.MethodPost, "/tasks/"+taskID+"/execute", `{"ai_employee_id":"`+employeeID+`"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestExecuteAuditFailsClosedWithoutSink(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, _ execution.AdmitRequest) (execution.AdmitResult, error) {
		return execution.AdmitResult{Execution: newExecution(execution.StatusPending), Created: true}, nil
	}}
	rec := serveExecution(t, svc, nil, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/tasks/"+taskID+"/execute", `{"ai_employee_id":"`+employeeID+`"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestExecuteAuditFailsClosedOnPersistError(t *testing.T) {
	svc := &fakeExecutionService{admit: func(_ context.Context, _ execution.AdmitRequest) (execution.AdmitResult, error) {
		return execution.AdmitResult{Execution: newExecution(execution.StatusPending), Created: true}, nil
	}}
	rec := serveExecution(t, svc, &failingSink{err: errors.New("sink down")}, execClaims(rbac.RoleWorkspaceAdmin, wsID),
		http.MethodPost, "/tasks/"+taskID+"/execute", `{"ai_employee_id":"`+employeeID+`"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}
