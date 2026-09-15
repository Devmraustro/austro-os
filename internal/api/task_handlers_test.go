package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/rbac"
	"austro-os/internal/task"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakeTaskStore is an in-memory, workspace-isolated task.TaskStore. The handler
// tests run the real task.Service on top of it, so the lifecycle rules being
// exercised are the production ones rather than a reimplementation that could
// agree with the handler by coincidence.
type fakeTaskStore struct {
	mu    sync.Mutex
	items map[uuid.UUID]*task.Task
}

func newFakeTaskStore() *fakeTaskStore {
	return &fakeTaskStore{items: map[uuid.UUID]*task.Task{}}
}

func (f *fakeTaskStore) Create(_ context.Context, t *task.Task) (*task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *t
	f.items[t.ID] = &cp
	out := *t
	return &out, nil
}

func (f *fakeTaskStore) Get(_ context.Context, ws, id uuid.UUID) (*task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.items[id]
	if !ok || t.WorkspaceID != ws {
		return nil, task.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (f *fakeTaskStore) List(_ context.Context, ws uuid.UUID, status *task.Status) ([]*task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*task.Task
	for _, t := range f.items {
		if t.WorkspaceID != ws {
			continue
		}
		if status != nil && t.Status != *status {
			continue
		}
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeTaskStore) ListPage(_ context.Context, ws uuid.UUID, q task.ListQuery) (task.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q.Normalize()
	var all []*task.Task
	for _, t := range f.items {
		if t.WorkspaceID != ws {
			continue
		}
		if q.Status != nil && t.Status != *q.Status {
			continue
		}
		cp := *t
		all = append(all, &cp)
	}
	// Newest first by (created_at, id), matching the SQL ordering.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0; j-- {
			a, b := all[j-1], all[j]
			less := a.CreatedAt.Before(b.CreatedAt) ||
				(a.CreatedAt.Equal(b.CreatedAt) && a.ID.String() < b.ID.String())
			if less {
				all[j-1], all[j] = all[j], all[j-1]
			}
		}
	}
	var out []*task.Task
	for _, t := range all {
		if q.Before.Set {
			after := t.CreatedAt.Before(q.Before.CreatedAt) ||
				(t.CreatedAt.Equal(q.Before.CreatedAt) && t.ID.String() < q.Before.ID.String())
			if !after {
				continue
			}
		}
		out = append(out, t)
	}
	page := task.Page{Limit: q.Limit}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		page.NextCursor = task.EncodeCursor(out[len(out)-1])
	}
	page.Tasks = out
	return page, nil
}

func (f *fakeTaskStore) Update(_ context.Context, ws uuid.UUID, t *task.Task) (*task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.items[t.ID]
	if !ok || cur.WorkspaceID != ws {
		return nil, task.ErrNotFound
	}
	cp := *t
	f.items[t.ID] = &cp
	out := *t
	return &out, nil
}

func (f *fakeTaskStore) Delete(_ context.Context, ws, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.items[id]
	if !ok || t.WorkspaceID != ws {
		return task.ErrNotFound
	}
	delete(f.items, id)
	return nil
}

// serveTask rides the same ordering as main.go: verified claims attached first,
// then the deny-by-default authorizer, then the handler mux. Route denial
// semantics therefore match the production server rather than being simulated.
func serveTask(t *testing.T, store *fakeTaskStore, sink audit.Sink, claims *auth.Claims, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	svc := task.NewService(store, nil, nil)
	h := NewTaskHandler(svc)
	if sink != nil {
		h = h.SetAuditSink(sink)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", h.Create)
	mux.HandleFunc("GET /tasks", h.List)
	mux.HandleFunc("GET /tasks/{id}", h.Get)
	mux.HandleFunc("PATCH /tasks/{id}", h.Update)
	mux.HandleFunc("POST /tasks/{id}/transition", h.Transition)

	az := authz.NewAuthorizer()
	az.AddRules(rbac.ImplementedRules())

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := az.Authorize(r, r.Method, r.URL.Path); err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})

	var rdr *bytes.Buffer
	if body == "" {
		rdr = bytes.NewBuffer(nil)
	} else {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if claims != nil {
		req = auth.WithClaims(req, claims)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// memberClaims is a workspace member bound to a tenant.
func memberClaims(ws string) *auth.Claims {
	return &auth.Claims{ID: "aaaaaaaa-0000-4000-8000-000000000001",
		Role: rbac.RoleWorkspaceMember, WorkspaceID: ws,
		Permissions: rbac.PermissionsForRole(rbac.RoleWorkspaceMember)}
}

func adminClaims(ws string) *auth.Claims {
	return &auth.Claims{ID: "bbbbbbbb-0000-4000-8000-000000000002",
		Role: rbac.RoleWorkspaceAdmin, WorkspaceID: ws,
		Permissions: rbac.PermissionsForRole(rbac.RoleWorkspaceAdmin)}
}

// createTaskViaAPI is a helper for the tests that need a task to exist first.
func createTaskViaAPI(t *testing.T, store *fakeTaskStore, claims *auth.Claims, title string) taskResponse {
	t.Helper()
	rec := serveTask(t, store, nil, claims, http.MethodPost, "/tasks",
		`{"title":"`+title+`","priority":"high"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out taskResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// TestTaskCreateIsConfinedToTheClaimsWorkspace is the central isolation
// assertion. The request names a different workspace in its body; the store must
// still receive the workspace from the verified token.
func TestTaskCreateIsConfinedToTheClaimsWorkspace(t *testing.T) {
	ws := uuid.NewString()
	other := uuid.NewString()
	store := newFakeTaskStore()

	// decodeJSON rejects unknown fields, so an attempt to inject a workspace is
	// refused outright rather than silently ignored.
	rec := serveTask(t, store, nil, memberClaims(ws), http.MethodPost, "/tasks",
		`{"title":"injected","workspace_id":"`+other+`"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a client-supplied workspace must be rejected, not honoured or ignored: %s", rec.Body.String())

	rec = serveTask(t, store, nil, memberClaims(ws), http.MethodPost, "/tasks",
		`{"title":"legitimate"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.items, 1)
	for _, tk := range store.items {
		require.Equal(t, ws, tk.WorkspaceID.String(),
			"the task must be owned by the workspace in the verified claims")
		require.NotEqual(t, other, tk.WorkspaceID.String())
	}
}

// TestTaskRoutesDenyWithoutWorkspaceContext proves the founder and any
// workspace-less identity are refused. The founder has no home workspace by
// database constraint, so there is no tenant to act in.
func TestTaskRoutesDenyWithoutWorkspaceContext(t *testing.T) {
	store := newFakeTaskStore()
	created := createTaskViaAPI(t, store, memberClaims(uuid.NewString()), "x")

	for name, claims := range map[string]*auth.Claims{
		"founder": workspaceClaims(rbac.RoleFounder, ""),
		"member with no binding": {ID: "user-x", Role: rbac.RoleWorkspaceMember,
			Permissions: rbac.PermissionsForRole(rbac.RoleWorkspaceMember)},
	} {
		for _, tc := range []struct {
			method, path, body string
		}{
			{http.MethodGet, "/tasks", ""},
			{http.MethodPost, "/tasks", `{"title":"t"}`},
			{http.MethodGet, "/tasks/" + created.ID, ""},
			{http.MethodPatch, "/tasks/" + created.ID, `{"title":"t"}`},
			{http.MethodPost, "/tasks/" + created.ID + "/transition", `{"status":"planned"}`},
		} {
			t.Run(name+" "+tc.method+" "+tc.path, func(t *testing.T) {
				rec := serveTask(t, store, nil, claims, tc.method, tc.path, tc.body)
				require.Equal(t, http.StatusForbidden, rec.Code,
					"%s %s must be denied without a workspace context", tc.method, tc.path)
			})
		}
	}
}

// TestTaskRoutesRequireAuthentication covers the unauthenticated path.
func TestTaskRoutesRequireAuthentication(t *testing.T) {
	store := newFakeTaskStore()
	rec := serveTask(t, store, nil, nil, http.MethodGet, "/tasks", "")
	require.Equal(t, http.StatusForbidden, rec.Code,
		"the deny-by-default authorizer refuses an unauthenticated caller")
}

// TestTaskGetCannotReachAnotherWorkspace is the IDOR case: a task id belonging
// to another tenant must not be readable, updatable or transitionable.
func TestTaskGetCannotReachAnotherWorkspace(t *testing.T) {
	wsA := uuid.NewString()
	wsB := uuid.NewString()
	store := newFakeTaskStore()

	ownedByB := createTaskViaAPI(t, store, memberClaims(wsB), "belongs to B")
	claimsA := memberClaims(wsA)

	rec := serveTask(t, store, nil, claimsA, http.MethodGet, "/tasks/"+ownedByB.ID, "")
	require.Equal(t, http.StatusNotFound, rec.Code,
		"workspace A must not read workspace B's task")

	rec = serveTask(t, store, nil, claimsA, http.MethodPatch, "/tasks/"+ownedByB.ID, `{"title":"stolen"}`)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"workspace A must not update workspace B's task")

	rec = serveTask(t, store, nil, claimsA, http.MethodPost, "/tasks/"+ownedByB.ID+"/transition",
		`{"status":"cancelled"}`)
	require.Equal(t, http.StatusNotFound, rec.Code,
		"workspace A must not transition workspace B's task")

	// And it must not appear in A's listing.
	rec = serveTask(t, store, nil, claimsA, http.MethodGet, "/tasks", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var page taskPage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Empty(t, page.Tasks, "workspace A's listing must not contain workspace B's task")
}

func TestTaskCreateValidation(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())

	for name, body := range map[string]string{
		"missing title":          `{"priority":"high"}`,
		"blank title":            `{"title":"   "}`,
		"empty title":            `{"title":""}`,
		"unknown priority":       `{"title":"t","priority":"critical"}`,
		"wrong type":             `{"title":123}`,
		"malformed json":         `{"title":`,
		"trailing json":          `{"title":"t"} {"title":"u"}`,
		"unknown field":          `{"title":"t","status":"completed"}`,
		"status is not settable": `{"title":"t","status":"completed"}`,
		"oversized title":        `{"title":"` + strings.Repeat("x", 201) + `"}`,
		"oversized description":  `{"title":"t","description":"` + strings.Repeat("y", 4001) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveTask(t, store, nil, claims, http.MethodPost, "/tasks", body)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be a 400, got %d: %s", name, rec.Code, rec.Body.String())
		})
	}
}

// TestTaskCreateRejectsOversizedBody covers the transport-level bound rather than
// the field bound: decodeJSON caps the body, so a huge payload is refused before
// any of it is parsed.
func TestTaskCreateRejectsOversizedBody(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	huge := `{"title":"` + strings.Repeat("z", maxRequestBodyBytes) + `"}`
	rec := serveTask(t, store, nil, claims, http.MethodPost, "/tasks", huge)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an oversized body must be refused, got %d", rec.Code)
}

func TestTaskCreateHappyPath(t *testing.T) {
	store := newFakeTaskStore()
	sink := &recordingSink{}
	claims := adminClaims(uuid.NewString())

	rec := serveTask(t, store, sink, claims, http.MethodPost, "/tasks",
		`{"title":"Write the script","description":"draft one","priority":"urgent"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var out taskResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "Write the script", out.Title)
	require.Equal(t, "draft one", out.Description)
	require.Equal(t, "urgent", out.Priority)
	require.Equal(t, "backlog", out.Status, "a new task is always backlog")
	require.Equal(t, claims.WorkspaceID, out.WorkspaceID)
	require.Equal(t, []string{"planned", "cancelled", "failed"}, out.Transitions,
		"backlog may move to planned, cancelled or failed")

	// The decision is on the audit chain with the real caller as actor.
	sink.mu.Lock()
	defer sink.mu.Unlock()
	require.NotEmpty(t, sink.records)
	rec0 := sink.records[len(sink.records)-1]
	require.Equal(t, "task.create", rec0.EventType)
	require.Equal(t, "success", rec0.Outcome)
	require.Equal(t, "user", rec0.ActorType)
	require.Equal(t, claims.ID, rec0.ActorID.String(),
		"the audit record must name the verified caller, not a generic system actor")
	require.NotNil(t, rec0.WorkspaceID)
	require.Equal(t, claims.WorkspaceID, rec0.WorkspaceID.String())
}

// TestTaskTransitionsAreServerAuthoritative is the lifecycle guarantee: the
// client names a destination and the server decides against the state it read.
func TestTaskTransitionsAreServerAuthoritative(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	created := createTaskViaAPI(t, store, claims, "lifecycle")

	transition := func(id, status string) *httptest.ResponseRecorder {
		return serveTask(t, store, nil, claims, http.MethodPost,
			"/tasks/"+id+"/transition", `{"status":"`+status+`"}`)
	}

	// backlog -> completed is not a legal move; it must be refused even though
	// both statuses exist.
	rec := transition(created.ID, "completed")
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"backlog cannot jump to completed: %s", rec.Body.String())

	// The legal path walks the lifecycle.
	for _, next := range []string{"planned", "in_progress", "in_review", "completed"} {
		rec = transition(created.ID, next)
		require.Equal(t, http.StatusOK, rec.Code, "moving to %s: %s", next, rec.Body.String())
		var out taskResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, next, out.Status)
	}

	// completed is terminal: nothing out of it is permitted, including a move
	// back to the state it just came from.
	for _, next := range []string{"backlog", "planned", "in_progress", "in_review",
		"completed", "cancelled", "rejected", "failed"} {
		rec = transition(created.ID, next)
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"no transition out of a terminal state is permitted (asked for %s): %s", next, rec.Body.String())
	}
}

func TestTaskTransitionValidation(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	created := createTaskViaAPI(t, store, claims, "validation")

	for name, body := range map[string]string{
		"missing status":         `{}`,
		"blank status":           `{"status":""}`,
		"unknown status":         `{"status":"archived"}`,
		"wrong case":             `{"status":"PLANNED"}`,
		"malformed json":         `{"status":`,
		"unknown field":          `{"status":"planned","from":"backlog"}`,
		"status is not a string": `{"status":7}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveTask(t, store, nil, claims, http.MethodPost,
				"/tasks/"+created.ID+"/transition", body)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be a 400, got %d: %s", name, rec.Code, rec.Body.String())
		})
	}
}

// TestTaskTransitionOfUnknownTask covers the not-found path.
func TestTaskTransitionOfUnknownTask(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	rec := serveTask(t, store, nil, claims, http.MethodPost,
		"/tasks/"+uuid.NewString()+"/transition", `{"status":"planned"}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestTaskMalformedIDIsClientError covers the path parameter.
func TestTaskMalformedIDIsClientError(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	// No spaces: httptest.NewRequest parses the target as a real request line,
	// so a value containing one is not a malformed id but a malformed request.
	// The injection-shaped cases are here for their content, not their spacing.
	for _, id := range []string{
		"not-a-uuid",
		"1",
		"00000000-0000-0000-0000-00000000000g",
		"';DROP+TABLE+tasks;--",
		"%27%20OR%201%3D1",
	} {
		rec := serveTask(t, store, nil, claims, http.MethodGet, "/tasks/"+id, "")
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"a malformed task id %q must be a 400, got %d", id, rec.Code)
	}
}

func TestTaskUpdatePermittedFieldsOnly(t *testing.T) {
	store := newFakeTaskStore()
	sink := &recordingSink{}
	claims := adminClaims(uuid.NewString())
	created := createTaskViaAPI(t, store, claims, "original")

	deadline := time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC).Format(time.RFC3339)
	assignee := uuid.NewString()
	rec := serveTask(t, store, sink, claims, http.MethodPatch, "/tasks/"+created.ID,
		`{"title":"renamed","description":"new body","priority":"low",`+
			`"assignee_type":"human","assignee_id":"`+assignee+`","deadline":"`+deadline+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out taskResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "renamed", out.Title)
	require.Equal(t, "new body", out.Description)
	require.Equal(t, "low", out.Priority)
	require.Equal(t, "human", out.AssigneeType)
	require.Equal(t, assignee, out.AssigneeID)
	require.Equal(t, deadline, out.Deadline)
	require.Equal(t, "backlog", out.Status, "a PATCH must not change the lifecycle state")
}

// TestTaskUpdateCannotChangeStatus is the property that keeps the lifecycle
// server-side: status is not a field PATCH accepts, so it cannot be used to move
// a task past the transition rules.
func TestTaskUpdateCannotChangeStatus(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	created := createTaskViaAPI(t, store, claims, "immutable status")

	rec := serveTask(t, store, nil, claims, http.MethodPatch, "/tasks/"+created.ID,
		`{"status":"completed"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"status is not an updatable field: %s", rec.Body.String())

	// The task is unchanged.
	rec = serveTask(t, store, nil, claims, http.MethodGet, "/tasks/"+created.ID, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var out taskResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "backlog", out.Status)
}

// TestTaskPriorityDefaultsAndBlanks pins the two priority rules, which differ
// between create and update on purpose.
func TestTaskPriorityDefaultsAndBlanks(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())

	t.Run("omitted on create means normal", func(t *testing.T) {
		rec := serveTask(t, store, nil, claims, http.MethodPost, "/tasks", `{"title":"t"}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var out taskResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, "normal", out.Priority,
			"priority is optional and defaults to normal")
	})

	t.Run("explicitly blank on update is refused", func(t *testing.T) {
		created := createTaskViaAPI(t, store, claims, "prio")
		rec := serveTask(t, store, nil, claims, http.MethodPatch, "/tasks/"+created.ID,
			`{"priority":""}`)
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"an explicit blank priority must not silently reset the field: %s", rec.Body.String())

		// And the task kept its priority.
		rec = serveTask(t, store, nil, claims, http.MethodGet, "/tasks/"+created.ID, "")
		require.Equal(t, http.StatusOK, rec.Code)
		var out taskResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, "high", out.Priority,
			"a refused update must not have changed anything")
	})

	t.Run("omitted on update leaves it alone", func(t *testing.T) {
		created := createTaskViaAPI(t, store, claims, "prio2")
		rec := serveTask(t, store, nil, claims, http.MethodPatch, "/tasks/"+created.ID,
			`{"title":"renamed"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out taskResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, "high", out.Priority)
	})
}

func TestTaskUpdateValidation(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	created := createTaskViaAPI(t, store, claims, "validation")

	for name, body := range map[string]string{
		"blank title":           `{"title":"  "}`,
		"oversized title":       `{"title":"` + strings.Repeat("x", 201) + `"}`,
		"oversized description": `{"description":"` + strings.Repeat("y", 4001) + `"}`,
		"unknown priority":      `{"priority":"whenever"}`,
		"unknown assignee":      `{"assignee_type":"robot"}`,
		"malformed assignee":    `{"assignee_id":"nope"}`,
		"malformed deadline":    `{"deadline":"next friday"}`,
		"malformed json":        `{"title":`,
		"unknown field":         `{"title":"t","workspace_id":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveTask(t, store, nil, claims, http.MethodPatch, "/tasks/"+created.ID, body)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be a 400, got %d: %s", name, rec.Code, rec.Body.String())
		})
	}
}

// TestTaskUpdatePartialLeavesOtherFieldsAlone checks that an absent field is not
// treated as a request to clear it.
func TestTaskUpdatePartialLeavesOtherFieldsAlone(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	created := createTaskViaAPI(t, store, claims, "keep me")

	rec := serveTask(t, store, nil, claims, http.MethodPatch, "/tasks/"+created.ID,
		`{"description":"only this changes"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out taskResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "keep me", out.Title, "an omitted title must be left alone")
	require.Equal(t, "high", out.Priority, "an omitted priority must be left alone")
	require.Equal(t, "only this changes", out.Description)
}

func TestTaskListBoundsAndFilters(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	for i := 0; i < 5; i++ {
		createTaskViaAPI(t, store, claims, "task")
	}

	t.Run("limit is clamped not rejected", func(t *testing.T) {
		rec := serveTask(t, store, nil, claims, http.MethodGet, "/tasks?limit=100000", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var page taskPage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
		require.Equal(t, task.MaxPageSize, page.Limit,
			"the limit must be reduced to the documented maximum")
	})

	t.Run("paging returns a cursor and then the rest", func(t *testing.T) {
		rec := serveTask(t, store, nil, claims, http.MethodGet, "/tasks?limit=2", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var first taskPage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &first))
		require.Len(t, first.Tasks, 2)
		require.NotEmpty(t, first.NextCursor, "more rows exist so a cursor must be offered")

		rec = serveTask(t, store, nil, claims, http.MethodGet,
			"/tasks?limit=2&cursor="+first.NextCursor, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var second taskPage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &second))
		require.Len(t, second.Tasks, 2)
		seen := map[string]bool{}
		for _, tk := range append(append([]taskResponse{}, first.Tasks...), second.Tasks...) {
			require.False(t, seen[tk.ID], "task %s appeared on two pages", tk.ID)
			seen[tk.ID] = true
		}
	})

	for name, query := range map[string]string{
		"unknown parameter":   "?workspace_id=" + uuid.NewString(),
		"misspelled filter":   "?statu=backlog",
		"unknown status":      "?status=archived",
		"non-numeric limit":   "?limit=abc",
		"negative limit":      "?limit=-1",
		"malformed cursor":    "?cursor=not-a-cursor",
		"injection in cursor": "?cursor=1;DROP+TABLE+tasks--.x",
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveTask(t, store, nil, claims, http.MethodGet, "/tasks"+query, "")
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be a 400, got %d: %s", name, rec.Code, rec.Body.String())
		})
	}

	t.Run("status filter narrows the page", func(t *testing.T) {
		created := createTaskViaAPI(t, store, claims, "to plan")
		rec := serveTask(t, store, nil, claims, http.MethodPost,
			"/tasks/"+created.ID+"/transition", `{"status":"planned"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = serveTask(t, store, nil, claims, http.MethodGet, "/tasks?status=planned", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var page taskPage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
		require.Len(t, page.Tasks, 1)
		require.Equal(t, created.ID, page.Tasks[0].ID)
	})
}

// TestTaskClientErrorsAreNeverServerErrors is the regression the audit capability
// shipped: a client's malformed input was classified as an internal server error
// because validation happened below the handler. Every bad input here must be a
// 4xx, so an operator is never sent investigating input validation as an outage.
func TestTaskClientErrorsAreNeverServerErrors(t *testing.T) {
	store := newFakeTaskStore()
	claims := memberClaims(uuid.NewString())
	created := createTaskViaAPI(t, store, claims, "errors")

	requests := []struct{ method, path, body string }{
		{http.MethodPost, "/tasks", `{"title":""}`},
		{http.MethodPost, "/tasks", `{"title":"t","priority":"nope"}`},
		{http.MethodPost, "/tasks", `{`},
		{http.MethodGet, "/tasks?limit=abc", ""},
		{http.MethodGet, "/tasks?cursor=bad", ""},
		{http.MethodGet, "/tasks/bad-id", ""},
		{http.MethodGet, "/tasks/" + uuid.NewString(), ""},
		{http.MethodPatch, "/tasks/" + created.ID, `{"title":"  "}`},
		{http.MethodPost, "/tasks/" + created.ID + "/transition", `{"status":"completed"}`},
		{http.MethodPost, "/tasks/" + created.ID + "/transition", `{"status":"nope"}`},
	}
	for _, rq := range requests {
		rec := serveTask(t, store, nil, claims, rq.method, rq.path, rq.body)
		require.GreaterOrEqual(t, rec.Code, 400)
		require.Less(t, rec.Code, 500,
			"%s %s is client input and must not be a server fault, got %d: %s",
			rq.method, rq.path, rec.Code, rec.Body.String())
	}
}

// TestTaskAuditRecordsDenialsAndFailures checks that the security-relevant
// outcomes are on the chain, not only the successes.
func TestTaskAuditRecordsDenialsAndFailures(t *testing.T) {
	store := newFakeTaskStore()
	sink := &recordingSink{}
	claims := memberClaims(uuid.NewString())
	created := createTaskViaAPI(t, store, claims, "audit")

	serveTask(t, store, sink, claims, http.MethodPost,
		"/tasks/"+created.ID+"/transition", `{"status":"completed"}`)
	serveTask(t, store, sink, workspaceClaims(rbac.RoleFounder, ""),
		http.MethodGet, "/tasks", "")

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var outcomes []string
	for _, r := range sink.records {
		outcomes = append(outcomes, r.EventType+"/"+r.Outcome)
	}
	require.Contains(t, outcomes, "task.transition/failed",
		"a refused lifecycle move is security-relevant and must be recorded")
}

// TestTaskHandlerRequiresService pins the constructor contract.
func TestTaskHandlerRequiresService(t *testing.T) {
	h := NewTaskHandler(task.NewService(newFakeTaskStore(), nil, nil))
	require.NotNil(t, h)
	require.NotNil(t, h.SetAuditSink(&recordingSink{}))
}
