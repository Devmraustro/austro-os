package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/knowledge"
	"austro-os/internal/rbac"

	"bytes"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests drive the real knowledge.Service over an in-memory store, so the
// production validation and workspace rules execute rather than a stub. The
// database is exercised separately in tests/knowledge_store_live_test.go; what
// is proven here is the HTTP contract: status codes, body shapes, what the
// caller may reach, and that every security-relevant outcome is audited.

// fakeKnowledgeStore implements knowledge.DocumentStore in memory.
type fakeKnowledgeStore struct {
	byID map[uuid.UUID]*knowledge.Document
	err  error
}

func newFakeKnowledgeStore() *fakeKnowledgeStore {
	return &fakeKnowledgeStore{byID: map[uuid.UUID]*knowledge.Document{}}
}

func (f *fakeKnowledgeStore) Upsert(_ context.Context, d *knowledge.Document) (*knowledge.Document, error) {
	if f.err != nil {
		return nil, f.err
	}
	cp := *d
	f.byID[d.ID] = &cp
	return &cp, nil
}

func (f *fakeKnowledgeStore) Update(_ context.Context, d *knowledge.Document) (*knowledge.Document, error) {
	if f.err != nil {
		return nil, f.err
	}
	existing, ok := f.byID[d.ID]
	if !ok || existing.WorkspaceID != d.WorkspaceID {
		return nil, knowledge.ErrNotFound
	}
	cp := *d
	*existing = cp
	return existing, nil
}

func (f *fakeKnowledgeStore) Get(_ context.Context, ws, id uuid.UUID) (*knowledge.Document, error) {
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.byID[id]
	if !ok || d.WorkspaceID != ws {
		return nil, knowledge.ErrNotFound
	}
	cp := *d
	return &cp, nil
}

func (f *fakeKnowledgeStore) List(_ context.Context, ws uuid.UUID, kind *knowledge.Kind) ([]*knowledge.Document, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*knowledge.Document
	for _, d := range f.byID {
		if d.WorkspaceID != ws {
			continue
		}
		if kind != nil && d.Kind != *kind {
			continue
		}
		cp := *d
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeKnowledgeStore) ListPage(_ context.Context, ws uuid.UUID, q knowledge.ListQuery) (knowledge.Page, error) {
	if f.err != nil {
		return knowledge.Page{}, f.err
	}
	q.Normalize()
	all, err := f.List(context.Background(), ws, q.Kind)
	if err != nil {
		return knowledge.Page{}, err
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID.String() > all[j].ID.String()
	})
	if q.Before.Set {
		var kept []*knowledge.Document
		for _, d := range all {
			after := d.CreatedAt.Before(q.Before.CreatedAt) ||
				(d.CreatedAt.Equal(q.Before.CreatedAt) && d.ID.String() < q.Before.ID.String())
			if after {
				kept = append(kept, d)
			}
		}
		all = kept
	}
	page := knowledge.Page{Limit: q.Limit}
	if len(all) > q.Limit {
		all = all[:q.Limit]
		page.NextCursor = knowledge.EncodeCursor(all[len(all)-1])
	}
	page.Documents = all
	return page, nil
}

func (f *fakeKnowledgeStore) Search(_ context.Context, ws uuid.UUID, _ []float32, kind *knowledge.Kind, limit int) ([]*knowledge.Document, error) {
	list, err := f.List(context.Background(), ws, kind)
	if err != nil {
		return nil, err
	}
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func (f *fakeKnowledgeStore) Delete(_ context.Context, ws, id uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	d, ok := f.byID[id]
	if !ok || d.WorkspaceID != ws {
		return knowledge.ErrNotFound
	}
	delete(f.byID, id)
	return nil
}

// stubEmbedder returns a fixed vector. The handler tests are not about
// embedding quality; tests/knowledge_store_live_test.go runs the real gateway.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, ws uuid.UUID, _ string, dim int) ([]float32, error) {
	if ws == uuid.Nil {
		return nil, knowledge.ErrWorkspaceMismatch
	}
	return make([]float32, dim), nil
}

// serveKnowledge wires the real service behind the same authorization ordering
// production uses: the authorizer runs before routing, so a route with no rule
// is refused rather than served.
func serveKnowledge(t *testing.T, store *fakeKnowledgeStore, sink audit.Sink,
	claims *auth.Claims, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	svc := knowledge.NewService(store, stubEmbedder{}, nil, nil, 0)
	h := NewKnowledgeHandler(svc)
	if sink != nil {
		h = h.SetAuditSink(sink)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /knowledge", h.Create)
	mux.HandleFunc("GET /knowledge", h.List)
	mux.HandleFunc("GET /knowledge/{id}", h.Get)
	mux.HandleFunc("PATCH /knowledge/{id}", h.Update)
	mux.HandleFunc("DELETE /knowledge/{id}", h.Delete)
	mux.HandleFunc("POST /knowledge/search", h.Search)

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

// founderClaims has no workspace, which is the point: the founder is
// organization level and every knowledge route is workspace-scoped.
func founderClaims() *auth.Claims {
	return &auth.Claims{ID: "cccccccc-0000-4000-8000-000000000003",
		Role: rbac.RoleFounder,
		// No WorkspaceID, mirroring what login mints for the founder.
		Permissions: rbac.PermissionsForRole(rbac.RoleFounder)}
}

// createDocViaAPI creates a document. It always attaches a working sink: a
// successful mutation now fails closed when its audit record cannot be written,
// so a helper that omitted the sink would get a 500 for a reason unrelated to
// whatever the calling test is checking.
func createDocViaAPI(t *testing.T, store *fakeKnowledgeStore, claims *auth.Claims, title string) knowledgeResponse {
	t.Helper()
	rec := serveKnowledge(t, store, &recordingSink{}, claims, http.MethodPost, "/knowledge",
		`{"title":"`+title+`","content":"body text"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out knowledgeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestKnowledgeCreateHappyPath(t *testing.T) {
	store := newFakeKnowledgeStore()
	sink := &recordingSink{}
	claims := memberClaims(uuid.NewString())

	rec := serveKnowledge(t, store, sink, claims, http.MethodPost, "/knowledge",
		`{"title":"Brand voice","content":"We write plainly.","kind":"style_guide"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var out knowledgeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "Brand voice", out.Title)
	require.Equal(t, "style_guide", out.Kind)
	require.Equal(t, claims.WorkspaceID, out.WorkspaceID,
		"the workspace must be the caller's, stamped by the server")

	// The embedding is server-derived and must not be echoed back.
	require.NotContains(t, rec.Body.String(), "embedding",
		"the response must not expose storage mechanics")

	require.Len(t, sink.records, 1, "creation must be audited")
	require.Equal(t, "knowledge.create", sink.records[0].EventType,
		"the event type must be knowledge.create, not a mangled variant")
	require.Equal(t, "success", sink.records[0].Outcome)
	require.Equal(t, "user", sink.records[0].ActorType,
		"the actor must be the person who acted, not the system")
}

// TestKnowledgeKindDefaults covers the exact defect found in the task contract:
// a field documented as optional must actually be optional.
func TestKnowledgeKindDefaults(t *testing.T) {
	store := newFakeKnowledgeStore()
	claims := memberClaims(uuid.NewString())

	rec := serveKnowledge(t, store, &recordingSink{}, claims, http.MethodPost, "/knowledge",
		`{"title":"no kind given","content":"body"}`)
	require.Equal(t, http.StatusCreated, rec.Code,
		"an omitted kind must default rather than fail: %s", rec.Body.String())
	var out knowledgeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "document", out.Kind)
}

func TestKnowledgeCreateValidation(t *testing.T) {
	store := newFakeKnowledgeStore()
	claims := memberClaims(uuid.NewString())

	cases := map[string]string{
		"missing title":   `{"content":"body"}`,
		"blank title":     `{"title":"   ","content":"body"}`,
		"missing content": `{"title":"t"}`,
		"blank content":   `{"title":"t","content":"  "}`,
		"unknown kind":    `{"title":"t","content":"b","kind":"note"}`,
		"unknown field":   `{"title":"t","content":"b","workspace_id":"other"}`,
		"malformed json":  `{"title":`,
		"empty body":      ``,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := serveKnowledge(t, store, nil, claims, http.MethodPost, "/knowledge", body)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be a 400, got %d: %s", name, rec.Code, rec.Body.String())
		})
	}
}

// TestKnowledgeWorkspaceIsNeverTakenFromTheRequest is the isolation property
// that matters most: there is no way to make the server write into a workspace
// other than the caller's.
func TestKnowledgeWorkspaceIsNeverTakenFromTheRequest(t *testing.T) {
	store := newFakeKnowledgeStore()
	claims := memberClaims(uuid.NewString())
	other := uuid.NewString()

	// A body field naming another workspace is an unknown field and is refused...
	rec := serveKnowledge(t, store, nil, claims, http.MethodPost, "/knowledge",
		`{"title":"t","content":"b","workspace_id":"`+other+`"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// ...and a query parameter is refused too.
	rec = serveKnowledge(t, store, nil, claims, http.MethodGet,
		"/knowledge?workspace_id="+other, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// The document that does get created is in the caller's own workspace.
	created := createDocViaAPI(t, store, claims, "mine")
	require.Equal(t, claims.WorkspaceID, created.WorkspaceID)
	for _, d := range store.byID {
		require.Equal(t, claims.WorkspaceID, d.WorkspaceID.String(),
			"no document may be written to another workspace")
	}
}

func TestKnowledgeCannotReachAnotherWorkspace(t *testing.T) {
	store := newFakeKnowledgeStore()
	claimsA := memberClaims(uuid.NewString())
	claimsB := memberClaims(uuid.NewString())
	created := createDocViaAPI(t, store, claimsA, "A's document")

	// 404, not 403: the identifier is the document, so 403 would confirm to
	// another tenant that it exists.
	rec := serveKnowledge(t, store, nil, claimsB, http.MethodGet, "/knowledge/"+created.ID, "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = serveKnowledge(t, store, nil, claimsB, http.MethodPatch, "/knowledge/"+created.ID,
		`{"title":"stolen"}`)
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = serveKnowledge(t, store, nil, claimsB, http.MethodDelete, "/knowledge/"+created.ID, "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	// And nothing changed.
	rec = serveKnowledge(t, store, nil, claimsA, http.MethodGet, "/knowledge/"+created.ID, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "A's document", out.Title)
}

func TestKnowledgeFounderHasNoWorkspace(t *testing.T) {
	store := newFakeKnowledgeStore()
	founder := founderClaims()

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/knowledge", `{"title":"t","content":"b"}`},
		{http.MethodGet, "/knowledge", ""},
		{http.MethodPost, "/knowledge/search", `{"query":"anything"}`},
	} {
		rec := serveKnowledge(t, store, nil, founder, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusForbidden, rec.Code,
			"%s %s must be refused for a founder with no workspace, got %d: %s",
			tc.method, tc.path, rec.Code, rec.Body.String())
	}
	require.Empty(t, store.byID, "nothing may be written")
}

// TestKnowledgeRoutesRefuseAnAnonymousCaller pins the status an unauthenticated
// request actually gets: 403, not 401. The deny-by-default authorizer runs before
// the handler, so a caller presenting nothing is refused for having no
// authorization at all and never reaches the authentication check inside.
// Asserting 401 here would be asserting a layering that does not exist.
func TestKnowledgeRoutesRefuseAnAnonymousCaller(t *testing.T) {
	store := newFakeKnowledgeStore()
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/knowledge", `{"title":"t","content":"b"}`},
		{http.MethodGet, "/knowledge", ""},
		{http.MethodGet, "/knowledge/" + uuid.NewString(), ""},
		{http.MethodPatch, "/knowledge/" + uuid.NewString(), `{"title":"x"}`},
		{http.MethodDelete, "/knowledge/" + uuid.NewString(), ""},
		{http.MethodPost, "/knowledge/search", `{"query":"x"}`},
	} {
		rec := serveKnowledge(t, store, nil, nil, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusForbidden, rec.Code,
			"%s %s without claims must be 403, got %d", tc.method, tc.path, rec.Code)
	}
	require.Empty(t, store.byID, "nothing may be written by an anonymous caller")
}

// TestKnowledgeUpdateSemantics covers the rules that make a PATCH safe: omitted
// fields are left alone, an empty PATCH is refused rather than silently
// accepted, and a blank value is not a way to clear a required field.
func TestKnowledgeUpdateSemantics(t *testing.T) {
	store := newFakeKnowledgeStore()
	claims := memberClaims(uuid.NewString())
	created := createDocViaAPI(t, store, claims, "original")

	t.Run("omitted fields are left alone", func(t *testing.T) {
		rec := serveKnowledge(t, store, &recordingSink{}, claims, http.MethodPatch, "/knowledge/"+created.ID,
			`{"title":"renamed"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out knowledgeResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, "renamed", out.Title)
		require.Equal(t, "body text", out.Content,
			"an omitted content field must not be cleared")
		require.Equal(t, "document", out.Kind)
	})

	t.Run("an empty PATCH is refused", func(t *testing.T) {
		rec := serveKnowledge(t, store, nil, claims, http.MethodPatch, "/knowledge/"+created.ID, `{}`)
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"returning 200 would claim something was written when nothing was")
	})

	t.Run("blank values are refused", func(t *testing.T) {
		for _, body := range []string{`{"title":"  "}`, `{"content":" "}`, `{"kind":""}`} {
			rec := serveKnowledge(t, store, nil, claims, http.MethodPatch,
				"/knowledge/"+created.ID, body)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be refused", body)
		}
	})

	t.Run("unknown kind is refused", func(t *testing.T) {
		rec := serveKnowledge(t, store, nil, claims, http.MethodPatch, "/knowledge/"+created.ID,
			`{"kind":"memo"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("a valid kind change succeeds", func(t *testing.T) {
		rec := serveKnowledge(t, store, &recordingSink{}, claims, http.MethodPatch, "/knowledge/"+created.ID,
			`{"kind":"campaign_rule"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out knowledgeResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, "campaign_rule", out.Kind)
	})

	t.Run("updating a missing document is 404", func(t *testing.T) {
		rec := serveKnowledge(t, store, nil, claims, http.MethodPatch,
			"/knowledge/"+uuid.NewString(), `{"title":"x"}`)
		require.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestKnowledgeDelete(t *testing.T) {
	store := newFakeKnowledgeStore()
	sink := &recordingSink{}
	claims := memberClaims(uuid.NewString())
	created := createDocViaAPI(t, store, claims, "to be deleted")

	rec := serveKnowledge(t, store, sink, claims, http.MethodDelete, "/knowledge/"+created.ID, "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = serveKnowledge(t, store, nil, claims, http.MethodGet, "/knowledge/"+created.ID, "")
	require.Equal(t, http.StatusNotFound, rec.Code, "the document must be gone")

	found := false
	for _, r := range sink.records {
		if r.EventType == "knowledge.delete" && r.Outcome == "success" {
			found = true
		}
	}
	require.True(t, found, "a deletion must be audited")

	// Deleting twice is 404, not a silent success.
	rec = serveKnowledge(t, store, nil, claims, http.MethodDelete, "/knowledge/"+created.ID, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestKnowledgeListBoundsAndFilters(t *testing.T) {
	store := newFakeKnowledgeStore()
	claims := memberClaims(uuid.NewString())
	for i := 0; i < 5; i++ {
		createDocViaAPI(t, store, claims, "doc")
	}

	t.Run("limit is honoured", func(t *testing.T) {
		rec := serveKnowledge(t, store, nil, claims, http.MethodGet, "/knowledge?limit=2", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var page knowledgePage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
		require.Len(t, page.Documents, 2)
		require.Equal(t, 2, page.Limit)
		require.NotEmpty(t, page.NextCursor)
	})

	t.Run("oversized limit is clamped and reported", func(t *testing.T) {
		rec := serveKnowledge(t, store, nil, claims, http.MethodGet, "/knowledge?limit=100000", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var page knowledgePage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
		require.Equal(t, knowledge.MaxPageSize, page.Limit)
	})

	t.Run("unknown parameter is refused", func(t *testing.T) {
		for _, q := range []string{
			"?workspace_id=other", "?offset=10", "?page=2", "?Cursor=x", "?limit=5&q=x",
		} {
			rec := serveKnowledge(t, store, nil, claims, http.MethodGet, "/knowledge"+q, "")
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be refused rather than ignored", q)
		}
	})

	t.Run("malformed values are refused", func(t *testing.T) {
		for _, q := range []string{
			"?limit=many", "?limit=-1", "?cursor=not-a-cursor", "?kind=memo",
		} {
			rec := serveKnowledge(t, store, nil, claims, http.MethodGet, "/knowledge"+q, "")
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be a 400, got %d", q, rec.Code)
		}
	})

	t.Run("a semicolon in the query string is refused", func(t *testing.T) {
		// url.ParseQuery rejects ";" as a separator, and r.URL.Query() would
		// have silently dropped the pair and served page one. Parsing the raw
		// query is what turns this into a 400.
		rec := serveKnowledge(t, store, nil, claims, http.MethodGet,
			"/knowledge?cursor=1;DROP+TABLE+knowledge_documents--", "")
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"a mangled query string must not be served as though it were empty")
	})

	t.Run("kind filter applies", func(t *testing.T) {
		rec := serveKnowledge(t, store, nil, claims, http.MethodGet,
			"/knowledge?kind=style_guide", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var page knowledgePage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
		require.Empty(t, page.Documents, "no style guides were created")
	})
}

func TestKnowledgeSearch(t *testing.T) {
	store := newFakeKnowledgeStore()
	sink := &recordingSink{}
	claims := memberClaims(uuid.NewString())
	createDocViaAPI(t, store, claims, "doc one")
	createDocViaAPI(t, store, claims, "doc two")

	t.Run("returns matches", func(t *testing.T) {
		rec := serveKnowledge(t, store, sink, claims, http.MethodPost, "/knowledge/search",
			`{"query":"anything","limit":5}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "embedding")
	})

	t.Run("empty query is refused", func(t *testing.T) {
		for _, body := range []string{`{"query":""}`, `{"query":"   "}`, `{}`} {
			rec := serveKnowledge(t, store, nil, claims, http.MethodPost, "/knowledge/search", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s must be refused", body)
		}
	})

	t.Run("oversized query is refused", func(t *testing.T) {
		rec := serveKnowledge(t, store, nil, claims, http.MethodPost, "/knowledge/search",
			`{"query":"`+strings.Repeat("x", 2001)+`"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"an unbounded query is a cheap way to make the embedder do unbounded work")
	})

	t.Run("limit out of range is refused", func(t *testing.T) {
		for _, body := range []string{`{"query":"x","limit":0}`, `{"query":"x","limit":-1}`,
			`{"query":"x","limit":5000}`} {
			rec := serveKnowledge(t, store, nil, claims, http.MethodPost, "/knowledge/search", body)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"%s must be rejected rather than silently defaulted", body)
		}
	})

	t.Run("unknown kind is refused", func(t *testing.T) {
		rec := serveKnowledge(t, store, nil, claims, http.MethodPost, "/knowledge/search",
			`{"query":"x","kind":"memo"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("search is audited", func(t *testing.T) {
		found := false
		for _, r := range sink.records {
			if r.EventType == "knowledge.search" && r.Outcome == "success" {
				found = true
			}
		}
		require.True(t, found, "a search reads the whole corpus and must be audited")
	})
}

func TestKnowledgeMalformedIDIsClientError(t *testing.T) {
	store := newFakeKnowledgeStore()
	claims := memberClaims(uuid.NewString())
	for _, id := range []string{"not-a-uuid", "1", "%27%20OR%201%3D1", "00000000-0000-0000-0000-00000000000g"} {
		rec := serveKnowledge(t, store, nil, claims, http.MethodGet, "/knowledge/"+id, "")
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"a malformed id %q must be a 400, got %d", id, rec.Code)
	}
}

// TestKnowledgeAuditFailsClosed covers the reason audit lives at the handler:
// knowledge.AuditSink.Record cannot report a failure, but auditstore.Append can,
// so a security-relevant outcome that cannot be recorded must not report success.
func TestKnowledgeAuditFailsClosed(t *testing.T) {
	store := newFakeKnowledgeStore()
	sink := &recordingSink{err: knowledge.ErrInvalidInput}
	claims := memberClaims(uuid.NewString())

	rec := serveKnowledge(t, store, sink, claims, http.MethodPost, "/knowledge",
		`{"title":"t","content":"b"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a create whose audit record could not be written must not report success")
}

// TestKnowledgeStoreFailureIsServerError separates client mistakes from server
// ones: a storage failure must be a 500, and its detail must not be echoed.
func TestKnowledgeStoreFailureIsServerError(t *testing.T) {
	store := newFakeKnowledgeStore()
	store.err = knowledge.ErrInvalidInput // stands in for an arbitrary store fault
	claims := memberClaims(uuid.NewString())

	rec := serveKnowledge(t, store, nil, claims, http.MethodGet, "/knowledge", "")
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a domain sentinel is classified as a client error")

	store2 := newFakeKnowledgeStore()
	store2.err = errStoreFault{}
	rec = serveKnowledge(t, store2, nil, claims, http.MethodGet, "/knowledge", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, strings.ToLower(rec.Body.String()), "boom",
		"an internal failure must not leak its detail to the client")
}

type errStoreFault struct{}

func (errStoreFault) Error() string { return "boom: connection refused" }

// TestKnowledgeCreatedAtIsStableAcrossUpdates guards the timestamp contract:
// updated_at moves, created_at does not.
func TestKnowledgeCreatedAtIsStableAcrossUpdates(t *testing.T) {
	store := newFakeKnowledgeStore()
	claims := memberClaims(uuid.NewString())
	created := createDocViaAPI(t, store, claims, "stable")

	rec := serveKnowledge(t, store, &recordingSink{}, claims, http.MethodPatch, "/knowledge/"+created.ID,
		`{"title":"changed"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out knowledgeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, created.CreatedAt, out.CreatedAt,
		"created_at must not move on an update")
	require.NotEqual(t, created.UpdatedAt, out.UpdatedAt,
		"updated_at must move")

	// And both are parseable timestamps, not raw database formats.
	_, err := time.Parse(time.RFC3339Nano, out.CreatedAt)
	require.NoError(t, err, "created_at must be RFC3339: %q", out.CreatedAt)
}
