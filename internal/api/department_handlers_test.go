package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/department"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeDeptStore struct {
	depts map[uuid.UUID]*department.Department
}

func newFakeDeptStore() *fakeDeptStore {
	return &fakeDeptStore{depts: map[uuid.UUID]*department.Department{}}
}

func (f *fakeDeptStore) Create(ctx context.Context, d *department.Department) (*department.Department, error) {
	f.depts[d.ID] = d
	return d, nil
}
func (f *fakeDeptStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*department.Department, error) {
	d, ok := f.depts[id]
	if !ok || d.WorkspaceID != workspaceID {
		return nil, department.ErrNotFound
	}
	return d, nil
}
func (f *fakeDeptStore) List(ctx context.Context, workspaceID uuid.UUID) ([]*department.Department, error) {
	var out []*department.Department
	for _, d := range f.depts {
		if d.WorkspaceID == workspaceID {
			out = append(out, d)
		}
	}
	return out, nil
}
func (f *fakeDeptStore) ListPage(ctx context.Context, workspaceID uuid.UUID, q department.ListQuery) (department.Page, error) {
	q.Normalize()
	list, _ := f.List(ctx, workspaceID)
	return department.Page{Departments: list, Limit: q.Limit}, nil
}
func (f *fakeDeptStore) Update(ctx context.Context, workspaceID uuid.UUID, d *department.Department) (*department.Department, error) {
	f.depts[d.ID] = d
	return d, nil
}
func (f *fakeDeptStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	delete(f.depts, id)
	return nil
}

func serveDepartment(t *testing.T, store *fakeDeptStore, sink audit.Sink, claims *auth.Claims, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	svc := department.NewService(store)
	h := NewDepartmentHandler(svc).SetAuditSink(sink)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /departments", h.Create)
	mux.HandleFunc("GET /departments", h.List)
	mux.HandleFunc("GET /departments/{id}", h.Get)
	mux.HandleFunc("PATCH /departments/{id}", h.Update)
	mux.HandleFunc("DELETE /departments/{id}", h.Delete)

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

func deptClaims(ws string) *auth.Claims {
	return &auth.Claims{
		ID:          uuid.NewString(),
		Role:        rbac.RoleWorkspaceAdmin,
		WorkspaceID: ws,
		Permissions: rbac.PermissionsForRole(rbac.RoleWorkspaceAdmin),
	}
}

func TestDepartmentCreateIsAudited(t *testing.T) {
	store := newFakeDeptStore()
	sink := &recordingSink{}
	ws := uuid.NewString()
	claims := deptClaims(ws)
	rec := serveDepartment(t, store, sink, claims, http.MethodPost, "/departments", `{"name":"audit-dept"}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Len(t, sink.records, 1)
	require.Equal(t, "department.create", sink.records[0].EventType)
	require.Equal(t, "success", sink.records[0].Outcome)
}

func TestDepartmentCreateRejectsBlankName(t *testing.T) {
	store := newFakeDeptStore()
	sink := &recordingSink{}
	ws := uuid.NewString()
	claims := deptClaims(ws)
	rec := serveDepartment(t, store, sink, claims, http.MethodPost, "/departments", `{"name":"   "}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDepartmentListIsWorkspaceScoped(t *testing.T) {
	store := newFakeDeptStore()
	sink := &recordingSink{}
	wsA := uuid.NewString()
	wsB := uuid.NewString()
	claimsA := deptClaims(wsA)
	// Create dept in A
	rec := serveDepartment(t, store, sink, claimsA, http.MethodPost, "/departments", `{"name":"dept-a"}`)
	require.Equal(t, http.StatusCreated, rec.Code)
	// Create dept in B directly via store
	deptB := &department.Department{ID: uuid.New(), WorkspaceID: uuid.MustParse(wsB), Name: "dept-b"}
	store.depts[deptB.ID] = deptB
	// List as A should not see B
	rec = serveDepartment(t, store, sink, claimsA, http.MethodGet, "/departments?limit=100", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "dept-b")
	require.Contains(t, rec.Body.String(), "dept-a")
}

func TestDepartmentCreateRequiresName(t *testing.T) {
	store := newFakeDeptStore()
	sink := &recordingSink{}
	ws := uuid.NewString()
	claims := deptClaims(ws)
	rec := serveDepartment(t, store, sink, claims, http.MethodPost, "/departments", `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
