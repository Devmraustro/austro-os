package austro_os_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This file drives the knowledge capability over the live HTTP stack, which is
// the only thing that proves the composition in main.go works: that the routes
// are registered, that the authorizer runs before them, that the handler reaches
// the real Postgres store through the unprivileged runtime role, that the AI
// gateway actually embeds, and that the audit sink records what happened.
//
// Sessions come from the shared helpers in audit_visibility_live_test.go. Login
// and bootstrap are rate limited per client address, and the whole suite issues
// its requests from one address inside a few seconds, so a test that
// authenticated for itself would exhaust the budget and make later, unrelated
// tests fail with 429.

// knowledgeLive mirrors the API envelope. It is named apart from the identically
// shaped type in package api because this package is flat.
type knowledgeLive struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type knowledgePageLive struct {
	Documents  []knowledgeLive `json:"documents"`
	NextCursor string          `json:"next_cursor"`
	Limit      int             `json:"limit"`
}

type knowledgeSearchLive struct {
	Documents []knowledgeLive `json:"documents"`
	Count     int             `json:"count"`
}

// createKnowledgeLive creates a document and returns the parsed response. Each
// title carries a unique marker so later assertions can find it without depending
// on how many rows the database already holds.
func createKnowledgeLive(t *testing.T, token, title, kind string) knowledgeLive {
	t.Helper()
	status, body := authJSONRaw(t, http.MethodPost, "/knowledge",
		fmt.Sprintf(`{"title":%q,"content":"live journey content for %s","kind":%q}`, title, title, kind),
		token)
	require.Equal(t, http.StatusCreated, status, "creation must succeed: %s", body)
	var out knowledgeLive
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, title, out.Title)
	require.Equal(t, kind, out.Kind)
	return out
}

// TestKnowledgeLiveJourney is the operator journey: create, read back, list,
// search, edit, then confirm the audit trail recorded what was done and by whom.
func TestKnowledgeLiveJourney(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	marker := "kn-" + uuid.NewString()[:8]

	created := createKnowledgeLive(t, tokens.memberA, marker, "style_guide")

	t.Run("the document belongs to the caller's workspace", func(t *testing.T) {
		require.NotEmpty(t, created.WorkspaceID,
			"the server must stamp the workspace; a client cannot choose one")
	})

	t.Run("the embedding is not exposed", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/knowledge/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		require.NotContains(t, string(body), "embedding",
			"the response must not expose storage mechanics")
	})

	t.Run("read back over HTTP", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/knowledge/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var got knowledgeLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, created.ID, got.ID)
		require.Equal(t, created.Title, got.Title)
		require.Equal(t, "style_guide", got.Kind)
		require.NotEmpty(t, got.Content)
	})

	t.Run("it appears in the listing", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/knowledge?limit=200", "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var page knowledgePageLive
		require.NoError(t, json.Unmarshal(body, &page))
		require.LessOrEqual(t, len(page.Documents), 200, "the page size must be honoured")
		require.Equal(t, 200, page.Limit, "the effective limit is reported")
		found := false
		for _, d := range page.Documents {
			if d.ID == created.ID {
				found = true
			}
			require.Equal(t, created.WorkspaceID, d.WorkspaceID,
				"the listing must contain only the caller's own workspace")
		}
		require.True(t, found, "the document just created must appear in the listing")
	})

	t.Run("the kind filter narrows the listing", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/knowledge?kind=style_guide&limit=200",
			"", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var page knowledgePageLive
		require.NoError(t, json.Unmarshal(body, &page))
		require.NotEmpty(t, page.Documents)
		for _, d := range page.Documents {
			require.Equal(t, "style_guide", d.Kind)
		}
	})

	t.Run("similarity search finds it", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPost, "/knowledge/search",
			`{"query":"live journey content","limit":20}`, tokens.memberA)
		require.Equal(t, http.StatusOK, status,
			"search must work against the embedded corpus: %s", body)
		var res knowledgeSearchLive
		require.NoError(t, json.Unmarshal(body, &res))
		require.NotEmpty(t, res.Documents,
			"the document was embedded on create, so a query close to its content must match")
		require.LessOrEqual(t, res.Count, 20)
		for _, d := range res.Documents {
			require.Equal(t, created.WorkspaceID, d.WorkspaceID,
				"search must not cross the workspace boundary")
		}
	})

	t.Run("editing preserves created_at and moves updated_at", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPatch, "/knowledge/"+created.ID,
			`{"title":"renamed by the journey test"}`, tokens.memberA)
		require.Equal(t, http.StatusOK, status, "an ordinary edit must succeed: %s", body)
		var got knowledgeLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "renamed by the journey test", got.Title)
		require.Equal(t, created.CreatedAt, got.CreatedAt, "created_at must not move")
		require.NotEqual(t, created.UpdatedAt, got.UpdatedAt, "updated_at must move")
	})

	t.Run("deleting removes it", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodDelete, "/knowledge/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusNoContent, status, body)

		status, body = authJSONRaw(t, http.MethodGet, "/knowledge/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusNotFound, status, "the document must be gone: %s", body)
	})

	t.Run("the audit trail recorded the journey", func(t *testing.T) {
		founder := auditFounderToken(t)
		for _, event := range []string{"knowledge.create", "knowledge.update", "knowledge.delete"} {
			status, body := authJSONRaw(t, http.MethodGet,
				"/audit/events?event_type="+event+"&limit=200", "", founder)
			require.Equal(t, http.StatusOK, status,
				"the founder must be able to read the org audit log: %s", body)
			var page auditPage
			require.NoError(t, json.Unmarshal(body, &page))
			require.NotEmpty(t, page.Events, "%s must have been audited", event)
			for _, ev := range page.Events {
				require.Equal(t, event, ev.EventType,
					"the event type must be exactly %s, not a mangled variant", event)
				require.Equal(t, "user", ev.ActorType,
					"the actor must be the person who acted, not the system")
			}
		}
	})
}

// TestKnowledgeLiveAuthorization covers who may reach the surface, and the
// boundary between tenants -- including the IDOR case where the identifier is
// guessed.
func TestKnowledgeLiveAuthorization(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	founder := auditFounderToken(t)
	created := createKnowledgeLive(t, tokens.memberA, "authz-"+uuid.NewString()[:8], "document")

	t.Run("anonymous requests are refused", func(t *testing.T) {
		// 403, not 401: the deny-by-default authorizer runs before
		// authentication, so a caller presenting nothing is refused for having no
		// authorization at all. An invalid token is different -- it reaches
		// authentication and fails there with 401.
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/knowledge", ""},
			{http.MethodPost, "/knowledge", `{"title":"x","content":"y"}`},
			{http.MethodGet, "/knowledge/" + created.ID, ""},
			{http.MethodPost, "/knowledge/search", `{"query":"x"}`},
		} {
			status, body := authJSONRaw(t, tc.method, tc.path, tc.body, "")
			require.Equal(t, http.StatusForbidden, status,
				"%s %s without a token must be 403, got %d: %s", tc.method, tc.path, status, body)

			status, body = authJSONRaw(t, tc.method, tc.path, tc.body, "garbage-token")
			require.Equal(t, http.StatusUnauthorized, status,
				"%s %s with an invalid token must be 401, got %d: %s", tc.method, tc.path, status, body)
		}
	})

	t.Run("the founder has no workspace and is refused", func(t *testing.T) {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/knowledge", ""},
			{http.MethodPost, "/knowledge", `{"title":"x","content":"y"}`},
			{http.MethodPost, "/knowledge/search", `{"query":"x"}`},
		} {
			status, body := authJSONRaw(t, tc.method, tc.path, tc.body, founder)
			require.Equal(t, http.StatusForbidden, status,
				"the founder is not attached to a workspace: %s", body)
		}
	})

	t.Run("another workspace cannot read it", func(t *testing.T) {
		// 404 rather than 403: the identifier is the document, so 403 would
		// confirm to another tenant that it exists.
		status, body := authJSONRaw(t, http.MethodGet, "/knowledge/"+created.ID, "", tokens.adminB)
		require.Equal(t, http.StatusNotFound, status,
			"cross-workspace access must not leak existence: %s", body)
	})

	t.Run("another workspace cannot modify or delete it", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPatch, "/knowledge/"+created.ID,
			`{"title":"stolen"}`, tokens.adminB)
		require.True(t, status == http.StatusNotFound || status == http.StatusForbidden,
			"cross-workspace update must be refused, got %d: %s", status, body)

		status, body = authJSONRaw(t, http.MethodDelete, "/knowledge/"+created.ID, "", tokens.adminB)
		require.True(t, status == http.StatusNotFound || status == http.StatusForbidden,
			"cross-workspace delete must be refused, got %d: %s", status, body)

		// And the owner can still read the original.
		status, body = authJSONRaw(t, http.MethodGet, "/knowledge/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var got knowledgeLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.NotEqual(t, "stolen", got.Title, "the other workspace must not have changed anything")
	})

	t.Run("a guessed identifier is refused the same way", func(t *testing.T) {
		status, _ := authJSONRaw(t, http.MethodGet, "/knowledge/"+uuid.NewString(), "", tokens.adminB)
		require.Equal(t, http.StatusNotFound, status,
			"a random id must be indistinguishable from another tenant's real one")
	})

	t.Run("the workspace admin can act on it too", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPatch, "/knowledge/"+created.ID,
			`{"kind":"campaign_rule"}`, tokens.adminA)
		require.Equal(t, http.StatusOK, status,
			"a workspace admin holds the same knowledge permissions: %s", body)
	})
}

// TestKnowledgeLiveInputValidation proves malformed input is a client error and
// never a 500. This is the class of defect the audit capability had, where an
// over-long filter surfaced as an internal error.
func TestKnowledgeLiveInputValidation(t *testing.T) {
	tokens := auditWorkspaceTokens(t)

	cases := map[string]struct {
		method string
		path   string
		body   string
	}{
		"missing title":         {http.MethodPost, "/knowledge", `{"content":"body"}`},
		"blank title":           {http.MethodPost, "/knowledge", `{"title":"   ","content":"b"}`},
		"missing content":       {http.MethodPost, "/knowledge", `{"title":"t"}`},
		"blank content":         {http.MethodPost, "/knowledge", `{"title":"t","content":"  "}`},
		"unknown kind":          {http.MethodPost, "/knowledge", `{"title":"t","content":"b","kind":"memo"}`},
		"unknown field":         {http.MethodPost, "/knowledge", `{"title":"t","content":"b","workspace_id":"x"}`},
		"malformed json":        {http.MethodPost, "/knowledge", `{"title":`},
		"empty body":            {http.MethodPost, "/knowledge", ``},
		"malformed document id": {http.MethodGet, "/knowledge/not-a-uuid", ""},
		"unknown query param":   {http.MethodGet, "/knowledge?workspace_id=other", ""},
		"non-numeric limit":     {http.MethodGet, "/knowledge?limit=many", ""},
		"negative limit":        {http.MethodGet, "/knowledge?limit=-1", ""},
		"unknown kind filter":   {http.MethodGet, "/knowledge?kind=memo", ""},
		"corrupt cursor":        {http.MethodGet, "/knowledge?cursor=not-a-cursor", ""},
		"semicolon in query":    {http.MethodGet, "/knowledge?cursor=1;DROP+TABLE+knowledge_documents--", ""},
		"empty search query":    {http.MethodPost, "/knowledge/search", `{"query":"  "}`},
		"search limit zero":     {http.MethodPost, "/knowledge/search", `{"query":"x","limit":0}`},
		"search limit huge":     {http.MethodPost, "/knowledge/search", `{"query":"x","limit":99999}`},
		"empty update":          {http.MethodPatch, "/knowledge/" + uuid.NewString(), `{}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := authJSONRaw(t, tc.method, tc.path, tc.body, tokens.memberA)
			require.True(t, status >= 400 && status < 500,
				"%s must be a client error, got %d: %s", name, status, body)
			require.NotContains(t, strings.ToLower(string(body)), "panic",
				"an internal failure must never be surfaced to the client")
		})
	}

	t.Run("an oversized limit is clamped rather than rejected", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/knowledge?limit=100000", "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var page knowledgePageLive
		require.NoError(t, json.Unmarshal(body, &page))
		require.LessOrEqual(t, page.Limit, 200,
			"the effective page size must be capped, and reported")
		require.LessOrEqual(t, len(page.Documents), page.Limit)
	})

	t.Run("oversized content is a client error", func(t *testing.T) {
		// 64 KiB is the domain cap; 70 KiB must be refused before it is embedded.
		status, body := authJSONRaw(t, http.MethodPost, "/knowledge",
			fmt.Sprintf(`{"title":"too big","content":%q}`, strings.Repeat("x", 70*1024)),
			tokens.memberA)
		require.True(t, status >= 400 && status < 500,
			"oversized content must be a client error, got %d: %s", status, body)
	})
}
