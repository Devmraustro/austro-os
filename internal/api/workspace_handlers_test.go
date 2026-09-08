package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/rbac"
	"austro-os/internal/workspace"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	wsATest = "11111111-1111-1111-1111-111111111111"
	wsBTest = "22222222-2222-2222-2222-222222222222"
)

// fakeWorkspaceStore is a deterministic in-memory workspace.Store used to
// exercise the workspace handlers without infrastructure.
type fakeWorkspaceStore struct {
	byID    map[uuid.UUID]*workspace.Workspace
	byName  map[string]uuid.UUID
	nextID  uuid.UUID
	errStep error
}

func newFakeWorkspaceStore() *fakeWorkspaceStore {
	f := &fakeWorkspaceStore{
		byID:   map[uuid.UUID]*workspace.Workspace{},
		byName: map[string]uuid.UUID{},
		nextID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
	}
	f.seed(wsATest, "Workspace A")
	f.seed(wsBTest, "Workspace B")
	return f
}

func (f *fakeWorkspaceStore) seed(id, name string) {
	uid := uuid.MustParse(id)
	w := &workspace.Workspace{ID: uid, Name: name, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
	f.byID[uid] = w
	f.byName[name] = uid
}

func (f *fakeWorkspaceStore) Create(_ context.Context, name string) (*workspace.Workspace, error) {
	if f.errStep != nil {
		return nil, f.errStep
	}
	if _, exists := f.byName[name]; exists {
		return nil, workspace.ErrNameTaken
	}
	id := f.nextID
	f.nextID = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	w := &workspace.Workspace{ID: id, Name: name, CreatedAt: time.Unix(2, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC()}
	f.byID[id] = w
	f.byName[name] = id
	return w, nil
}

func (f *fakeWorkspaceStore) Get(_ context.Context, id uuid.UUID) (*workspace.Workspace, error) {
	if f.errStep != nil {
		return nil, f.errStep
	}
	w, ok := f.byID[id]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return w, nil
}

func (f *fakeWorkspaceStore) List(_ context.Context) ([]*workspace.Workspace, error) {
	if f.errStep != nil {
		return nil, f.errStep
	}
	out := make([]*workspace.Workspace, 0, len(f.byID))
	for _, w := range f.byID {
		out = append(out, w)
	}
	return out, nil
}

// workspaceClaims builds verified claims for the given role/workspace with the
// role's explicit permissions, mirroring what the API mints at login.
func workspaceClaims(role rbac.Role, workspaceID string) *auth.Claims {
	return &auth.Claims{ID: "user-1", Role: role, WorkspaceID: workspaceID,
		Permissions: rbac.PermissionsForRole(role)}
}

// serveWorkspace rides the same ordering as main.go: verified claims attached
// first (RequireAuth), then the deny-by-default authorizer, then the handler
// mux — so route denial semantics match the production server.
func serveWorkspace(t *testing.T, f *fakeWorkspaceStore, claims *auth.Claims, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewWorkspaceHandler(f)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces", h.List)
	mux.HandleFunc("POST /workspaces", h.Create)
	mux.HandleFunc("GET /workspaces/{id}", h.Get)

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

func TestWorkspaceListFounderOnly(t *testing.T) {
	f := newFakeWorkspaceStore()
	founder := workspaceClaims(rbac.RoleFounder, "")
	rec := serveWorkspace(t, f, founder, http.MethodGet, "/workspaces", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var out []workspaceResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out, 2)
}

func TestWorkspaceListDeniedForNonFounder(t *testing.T) {
	f := newFakeWorkspaceStore()
	for _, tc := range []struct {
		name string
		role rbac.Role
	}{
		{"admin", rbac.RoleWorkspaceAdmin},
		{"member", rbac.RoleWorkspaceMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := workspaceClaims(tc.role, wsATest)
			rec := serveWorkspace(t, f, c, http.MethodGet, "/workspaces", "")
			require.Equal(t, http.StatusForbidden, rec.Code)
		})
	}
	// Unauthenticated: deny-by-default before the handler.
	rec := serveWorkspace(t, f, nil, http.MethodGet, "/workspaces", "")
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestWorkspaceCreateFounderSuccess(t *testing.T) {
	f := newFakeWorkspaceStore()
	founder := workspaceClaims(rbac.RoleFounder, "")
	rec := serveWorkspace(t, f, founder, http.MethodPost, "/workspaces",
		`{"name":"New Workspace"}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	var out workspaceResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, "New Workspace", out.Name)
	require.NotEmpty(t, out.CreatedAt)
	require.NotEmpty(t, out.UpdatedAt)
}

func TestWorkspaceCreateValidation(t *testing.T) {
	f := newFakeWorkspaceStore()
	founder := workspaceClaims(rbac.RoleFounder, "")

	cases := []struct {
		name string
		body string
	}{
		{"empty name", `{"name":""}`},
		{"whitespace name", `{"name":"   "}`},
		{"missing name", `{}`},
		{"too long name", `{"name":"` + strings.Repeat("A", workspaceNameMaxRunes+1) + `"}`},
		{"unknown field workspace_id", `{"name":"ok","workspace_id":"` + wsBTest + `"}`},
		{"malformed json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveWorkspace(t, f, founder, http.MethodPost, "/workspaces", tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestWorkspaceCreateDuplicateConflict(t *testing.T) {
	f := newFakeWorkspaceStore()
	founder := workspaceClaims(rbac.RoleFounder, "")

	first := serveWorkspace(t, f, founder, http.MethodPost, "/workspaces", `{"name":"Dup"}`)
	require.Equal(t, http.StatusCreated, first.Code)

	second := serveWorkspace(t, f, founder, http.MethodPost, "/workspaces", `{"name":"Dup"}`)
	require.Equal(t, http.StatusConflict, second.Code)
}

func TestWorkspaceCreateDeniedForNonFounder(t *testing.T) {
	f := newFakeWorkspaceStore()
	for _, tc := range []struct {
		name string
		role rbac.Role
	}{
		{"admin", rbac.RoleWorkspaceAdmin},
		{"member", rbac.RoleWorkspaceMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := workspaceClaims(tc.role, wsATest)
			rec := serveWorkspace(t, f, c, http.MethodPost, "/workspaces", `{"name":"x"}`)
			require.Equal(t, http.StatusForbidden, rec.Code)
		})
	}
	rec := serveWorkspace(t, f, nil, http.MethodPost, "/workspaces", `{"name":"x"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestWorkspaceGetFounderAnyWorkspace(t *testing.T) {
	f := newFakeWorkspaceStore()
	founder := workspaceClaims(rbac.RoleFounder, "")

	rec := serveWorkspace(t, f, founder, http.MethodGet, "/workspaces/"+wsATest, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var out workspaceResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, wsATest, out.ID)
	require.Equal(t, "Workspace A", out.Name)

	recB := serveWorkspace(t, f, founder, http.MethodGet, "/workspaces/"+wsBTest, "")
	require.Equal(t, http.StatusOK, recB.Code)
}

func TestWorkspaceGetAdminOwnOnly(t *testing.T) {
	f := newFakeWorkspaceStore()
	admin := workspaceClaims(rbac.RoleWorkspaceAdmin, wsATest)

	rec := serveWorkspace(t, f, admin, http.MethodGet, "/workspaces/"+wsATest, "")
	require.Equal(t, http.StatusOK, rec.Code)

	// A divergent path is refused by the ScopePath rule before the handler.
	recB := serveWorkspace(t, f, admin, http.MethodGet, "/workspaces/"+wsBTest, "")
	require.Equal(t, http.StatusForbidden, recB.Code)
}

func TestWorkspaceGetMemberDenied(t *testing.T) {
	f := newFakeWorkspaceStore()
	member := workspaceClaims(rbac.RoleWorkspaceMember, wsATest)
	rec := serveWorkspace(t, f, member, http.MethodGet, "/workspaces/"+wsATest, "")
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestWorkspaceGetFounderInvalidID(t *testing.T) {
	f := newFakeWorkspaceStore()
	founder := workspaceClaims(rbac.RoleFounder, "")
	rec := serveWorkspace(t, f, founder, http.MethodGet, "/workspaces/not-a-uuid", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWorkspaceGetNotFound(t *testing.T) {
	f := newFakeWorkspaceStore()
	founder := workspaceClaims(rbac.RoleFounder, "")
	rec := serveWorkspace(t, f, founder, http.MethodGet, "/workspaces/"+uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWorkspaceHandlerServerErrorIsGeneric(t *testing.T) {
	f := newFakeWorkspaceStore()
	f.errStep = errors.New("SELECT boom: syntax error at or near X")
	founder := workspaceClaims(rbac.RoleFounder, "")

	rec := serveWorkspace(t, f, founder, http.MethodGet, "/workspaces", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), "internal error")
	require.NotContains(t, rec.Body.String(), "boom", "SQL/cause detail must never leak")
}
