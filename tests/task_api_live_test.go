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

// This file drives the task capability over the live HTTP stack, which is the
// only thing that proves the composition in main.go actually works: that the
// routes are registered, that the authorizer runs before them, that the handler
// reaches the real Postgres store through the unprivileged runtime role, and
// that the audit sink records what happened. The handler tests in internal/api
// exercise the handler in isolation and cannot see a route that was never wired
// up or an RBAC rule that was never granted.
//
// Sessions come from the shared helpers in audit_visibility_live_test.go rather
// than being established here. Login and bootstrap are rate limited per client
// address, and the whole suite issues its requests from one address inside a few
// seconds, so a test that authenticated for itself would exhaust the budget and
// make later, unrelated tests fail with 429.

// taskLive mirrors the API's task envelope. It is named apart from the
// identically shaped type in package api because this package is flat and a
// second definition of the same name would not compile.
type taskLive struct {
	ID           string   `json:"id"`
	WorkspaceID  string   `json:"workspace_id"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Status       string   `json:"status"`
	Priority     string   `json:"priority"`
	AssigneeType string   `json:"assignee_type"`
	AssigneeID   string   `json:"assignee_id"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	Transitions  []string `json:"transitions"`
}

type taskPageLive struct {
	Tasks      []taskLive `json:"tasks"`
	NextCursor string     `json:"next_cursor"`
	Limit      int        `json:"limit"`
}

// createTaskLive creates a task and returns the parsed response. Every task this
// file creates carries a unique marker in its title so the audit assertions can
// find it again without depending on how many rows the database already holds.
func createTaskLive(t *testing.T, token, title string) taskLive {
	t.Helper()
	status, body := authJSONRaw(t, http.MethodPost, "/tasks",
		fmt.Sprintf(`{"title":%q,"description":"created by the live journey test","priority":"high"}`, title),
		token)
	require.Equal(t, http.StatusCreated, status, "task creation must succeed: %s", body)
	var out taskLive
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out.ID)
	require.Equal(t, title, out.Title)
	require.Equal(t, "backlog", out.Status, "a new task starts in backlog")
	require.Equal(t, "high", out.Priority)
	return out
}

// TestTaskLiveJourney is the full operator journey: create, read back, list,
// move the task forward through its lifecycle, then confirm the audit trail
// recorded what was done and by whom.
func TestTaskLiveJourney(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	marker := "journey-" + uuid.NewString()[:8]

	created := createTaskLive(t, tokens.memberA, marker)

	t.Run("the task belongs to the caller's workspace", func(t *testing.T) {
		require.NotEmpty(t, created.WorkspaceID,
			"the server must stamp the workspace; a client cannot choose one")
	})

	t.Run("read back over HTTP", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/tasks/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, "the creator must read the task back: %s", body)
		var got taskLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, created.ID, got.ID)
		require.Equal(t, created.Title, got.Title)
		require.Equal(t, "backlog", got.Status)
	})

	t.Run("it appears in the listing", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/tasks?limit=200", "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var page taskPageLive
		require.NoError(t, json.Unmarshal(body, &page))
		require.LessOrEqual(t, len(page.Tasks), 200, "the page size must be honoured")
		found := false
		for _, tk := range page.Tasks {
			if tk.ID == created.ID {
				found = true
			}
			require.Equal(t, created.WorkspaceID, tk.WorkspaceID,
				"the listing must contain only the caller's own workspace")
		}
		require.True(t, found, "the task just created must appear in the listing")
	})

	t.Run("backlog offers exactly the documented moves", func(t *testing.T) {
		require.ElementsMatch(t, []string{"planned", "cancelled", "failed"},
			created.Transitions,
			"the legal transitions come from the server, and from backlog they are fixed")
	})

	t.Run("walk the lifecycle", func(t *testing.T) {
		for _, next := range []string{"planned", "in_progress", "in_review", "completed"} {
			status, body := authJSONRaw(t, http.MethodPost, "/tasks/"+created.ID+"/transition",
				fmt.Sprintf(`{"status":%q}`, next), tokens.memberA)
			require.Equal(t, http.StatusOK, status,
				"backlog->planned->in_progress->in_review->completed must be legal; %s failed: %s", next, body)
			var got taskLive
			require.NoError(t, json.Unmarshal(body, &got))
			require.Equal(t, next, got.Status, "the server confirms the new status")
		}
	})

	t.Run("a completed task has nowhere left to go", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/tasks/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var got taskLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "completed", got.Status)
		require.Empty(t, got.Transitions,
			"completed is terminal: the server must offer no further move")
	})

	t.Run("the audit trail recorded the journey", func(t *testing.T) {
		founder := auditFounderToken(t)
		status, body := authJSONRaw(t, http.MethodGet,
			"/audit/events?event_type=task.transition&limit=200", "", founder)
		require.Equal(t, http.StatusOK, status,
			"the founder must be able to read the org audit log: %s", body)
		var page auditPage
		require.NoError(t, json.Unmarshal(body, &page))
		require.NotEmpty(t, page.Events, "the lifecycle walk must have been audited")
		for _, ev := range page.Events {
			require.Equal(t, "task.transition", ev.EventType)
			require.Equal(t, "user", ev.ActorType,
				"the actor must be the person who acted, not the system")
			require.Equal(t, "task", ev.TargetType)
		}

		// The create event carries the same actor type, which is what makes the
		// trail attributable rather than a log of anonymous changes.
		status, body = authJSONRaw(t, http.MethodGet,
			"/audit/events?event_type=task.create&limit=200", "", founder)
		require.Equal(t, http.StatusOK, status, body)
		var creates auditPage
		require.NoError(t, json.Unmarshal(body, &creates))
		require.NotEmpty(t, creates.Events, "task creation must be audited")
		for _, ev := range creates.Events {
			require.Equal(t, "task.create", ev.EventType,
				"the event type must be task.create, not a mangled variant")
		}
	})
}

// TestTaskLiveTransitionsAreServerAuthoritative proves the state machine cannot
// be talked around from the outside. This is the requirement that a client
// status is never trusted: the request names a destination, the server reads the
// current status itself, and a move the lifecycle forbids is refused.
func TestTaskLiveTransitionsAreServerAuthoritative(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	created := createTaskLive(t, tokens.memberA, "authority-"+uuid.NewString()[:8])

	t.Run("a shortcut is refused", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPost, "/tasks/"+created.ID+"/transition",
			`{"status":"completed"}`, tokens.memberA)
		require.True(t, status >= 400 && status < 500,
			"backlog -> completed skips the lifecycle and must be a client error, got %d: %s", status, body)

		// And the task did not move.
		status, body = authJSONRaw(t, http.MethodGet, "/tasks/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var got taskLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "backlog", got.Status, "a refused transition must change nothing")
	})

	t.Run("status cannot be set through PATCH", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPatch, "/tasks/"+created.ID,
			`{"status":"completed"}`, tokens.memberA)
		require.True(t, status >= 400 && status < 500,
			"PATCH must not be able to move the task: %d %s", status, body)

		status, body = authJSONRaw(t, http.MethodGet, "/tasks/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var got taskLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "backlog", got.Status,
			"the task must still be in backlog")
	})

	t.Run("an unknown status is a client error", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPost, "/tasks/"+created.ID+"/transition",
			`{"status":"archived"}`, tokens.memberA)
		require.True(t, status >= 400 && status < 500,
			"an invented status must not be a server error: %d %s", status, body)
	})

	t.Run("a PATCH that edits fields still works", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPatch, "/tasks/"+created.ID,
			`{"title":"renamed by the journey test","priority":"urgent"}`, tokens.memberA)
		require.Equal(t, http.StatusOK, status, "an ordinary field update must succeed: %s", body)
		var got taskLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "renamed by the journey test", got.Title)
		require.Equal(t, "urgent", got.Priority)
		require.Equal(t, "backlog", got.Status, "editing fields must not move the task")
	})
}

// TestTaskLiveAuthorization covers who may reach the surface at all, and the
// boundary between tenants.
func TestTaskLiveAuthorization(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	founder := auditFounderToken(t)
	created := createTaskLive(t, tokens.memberA, "authz-"+uuid.NewString()[:8])

	t.Run("anonymous requests are refused", func(t *testing.T) {
		// 403, not 401: the deny-by-default authorizer runs before
		// authentication, so a caller presenting nothing is refused for having
		// no authorization at all. An invalid token is different -- it reaches
		// authentication and fails there with 401. Both are asserted, because
		// collapsing them would hide which layer answered.
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/tasks", ""},
			{http.MethodPost, "/tasks", `{"title":"x"}`},
			{http.MethodGet, "/tasks/" + created.ID, ""},
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
		// Tasks are workspace-scoped. The founder is deliberately organization
		// level with no home workspace, so there is no scope to serve and the
		// server refuses rather than widening to every tenant.
		status, body := authJSONRaw(t, http.MethodGet, "/tasks", "", founder)
		require.Equal(t, http.StatusForbidden, status,
			"the founder is not attached to a workspace: %s", body)
		status, body = authJSONRaw(t, http.MethodPost, "/tasks", `{"title":"x"}`, founder)
		require.Equal(t, http.StatusForbidden, status, body)
	})

	t.Run("another workspace cannot read the task", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodGet, "/tasks/"+created.ID, "", tokens.adminB)
		// 404 rather than 403: the identifier is the task, not the workspace, so
		// answering 403 would confirm the task exists to a caller who has no
		// business knowing that.
		require.Equal(t, http.StatusNotFound, status,
			"cross-workspace access must not leak existence: %s", body)
	})

	t.Run("another workspace cannot move the task", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPost, "/tasks/"+created.ID+"/transition",
			`{"status":"planned"}`, tokens.adminB)
		require.True(t, status == http.StatusNotFound || status == http.StatusForbidden,
			"cross-workspace transition must be refused, got %d: %s", status, body)

		status, body = authJSONRaw(t, http.MethodGet, "/tasks/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var got taskLive
		require.NoError(t, json.Unmarshal(body, &got))
		require.Equal(t, "backlog", got.Status,
			"the other workspace must not have changed anything")
	})

	t.Run("another workspace cannot delete it either", func(t *testing.T) {
		// There is no DELETE route at all: a task is retired by moving it to a
		// terminal status, so history is never destroyed.
		status, _ := authJSONRaw(t, http.MethodDelete, "/tasks/"+created.ID, "", tokens.adminB)
		require.True(t, status == http.StatusMethodNotAllowed ||
			status == http.StatusNotFound || status == http.StatusForbidden,
			"there is no delete route; got %d", status)

		status, _ = authJSONRaw(t, http.MethodGet, "/tasks/"+created.ID, "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, "the task must still exist")
	})

	t.Run("the workspace admin can act on it too", func(t *testing.T) {
		status, body := authJSONRaw(t, http.MethodPost, "/tasks/"+created.ID+"/transition",
			`{"status":"planned"}`, tokens.adminA)
		require.Equal(t, http.StatusOK, status,
			"a workspace admin holds the same task permissions: %s", body)
	})
}

// TestTaskLiveInputValidation proves malformed input is a client error and never
// a 500, which is where an unhandled value usually ends up.
func TestTaskLiveInputValidation(t *testing.T) {
	tokens := auditWorkspaceTokens(t)

	cases := map[string]struct {
		method string
		path   string
		body   string
	}{
		"missing title":         {http.MethodPost, "/tasks", `{"description":"no title"}`},
		"blank title":           {http.MethodPost, "/tasks", `{"title":"   "}`},
		"unknown priority":      {http.MethodPost, "/tasks", `{"title":"x","priority":"whenever"}`},
		"unknown field":         {http.MethodPost, "/tasks", `{"title":"x","is_admin":true}`},
		"malformed json":        {http.MethodPost, "/tasks", `{"title":`},
		"empty body":            {http.MethodPost, "/tasks", ``},
		"malformed task id":     {http.MethodGet, "/tasks/not-a-uuid", ""},
		"unknown query param":   {http.MethodGet, "/tasks?workspace_id=other", ""},
		"non-numeric limit":     {http.MethodGet, "/tasks?limit=many", ""},
		"unknown status filter": {http.MethodGet, "/tasks?status=archived", ""},
		"corrupt cursor":        {http.MethodGet, "/tasks?cursor=not-a-cursor", ""},
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
		status, body := authJSONRaw(t, http.MethodGet, "/tasks?limit=100000", "", tokens.memberA)
		require.Equal(t, http.StatusOK, status, body)
		var page taskPageLive
		require.NoError(t, json.Unmarshal(body, &page))
		require.LessOrEqual(t, page.Limit, 200,
			"the effective page size must be capped, and reported")
		require.LessOrEqual(t, len(page.Tasks), page.Limit)
	})
}

// TestTaskLiveClientSuppliedWorkspaceIsIgnored proves the create path takes the
// workspace from the verified token and never from the request.
//
// The listing path already refuses a workspace_id query parameter (it is one of
// the cases in TestTaskLiveInputValidation). This covers the write vector, which
// is the one that could actually place a row inside another tenant.
//
// The value is a well-formed uuid, so the refusal is a policy decision rather
// than a parse failure -- a malformed id would prove only that the decoder
// rejects nonsense.
func TestTaskLiveClientSuppliedWorkspaceIsIgnored(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	title := "cross-tenant-" + uuid.NewString()[:8]

	status, body := authJSONRaw(t, http.MethodPost, "/tasks",
		fmt.Sprintf(`{"title":%q,"workspace_id":%q}`, title, uuid.NewString()),
		tokens.memberA)
	require.True(t, status >= 400 && status < 500,
		"a client-supplied workspace must be refused with a client error, got %d: %s",
		status, body)

	// The refusal must also be real: nothing was written under the caller's own
	// workspace either, so a rejected request cannot leave a stray row behind.
	status, body = authJSONRaw(t, http.MethodGet, "/tasks?limit=200", "", tokens.memberA)
	require.Equal(t, http.StatusOK, status, body)
	var page taskPageLive
	require.NoError(t, json.Unmarshal(body, &page))
	for _, existing := range page.Tasks {
		require.NotEqual(t, title, existing.Title,
			"a refused create must not have written a task")
	}
}

// TestTaskLiveFailedMutationIsNotAuditedAsSuccess pins the fail-closed half of
// the audit contract.
//
// A refused write must leave a record saying it was refused, and must not add a
// success row for a change that never happened. That distinction is the whole
// value of the trail: an audit log that records intent as achievement is worse
// than no log at all, because it is believed. The existing journey test proves a
// successful mutation is audited; this proves the other direction, which a
// success-only reading of the same table cannot detect.
func TestTaskLiveFailedMutationIsNotAuditedAsSuccess(t *testing.T) {
	tokens := auditWorkspaceTokens(t)
	founder := auditFounderToken(t)

	task := createTaskLive(t, tokens.memberA, "refused-"+uuid.NewString()[:8])

	successBefore := countTaskTransitionAudits(t, founder, "success")
	failedBefore := countTaskTransitionAudits(t, founder, "failed")

	// backlog -> completed skips three lifecycle stages. The domain refuses it,
	// and the handler classifies the refusal as the client's mistake.
	status, body := authJSONRaw(t, http.MethodPost, "/tasks/"+task.ID+"/transition",
		`{"status":"completed"}`, tokens.memberA)
	require.Equal(t, http.StatusBadRequest, status,
		"a shortcut through the lifecycle must be refused: %s", body)

	// Guard against the assertion below going blind rather than passing: a full
	// page cannot show a delta.
	require.Less(t, successBefore, 200,
		"the audit page is saturated, so this test can no longer observe a change")

	require.Equal(t, successBefore, countTaskTransitionAudits(t, founder, "success"),
		"a refused transition must not add a success audit row")
	require.Equal(t, failedBefore+1, countTaskTransitionAudits(t, founder, "failed"),
		"the refusal itself must be recorded, so the omission is visible in the trail")

	// The stored task is the fact a misleading success row would have denied.
	status, body = authJSONRaw(t, http.MethodGet, "/tasks/"+task.ID, "", tokens.memberA)
	require.Equal(t, http.StatusOK, status, body)
	var after taskLive
	require.NoError(t, json.Unmarshal(body, &after))
	require.Equal(t, "backlog", after.Status,
		"a refused transition must leave the stored status alone")
	require.ElementsMatch(t, []string{"planned", "cancelled", "failed"}, after.Transitions,
		"the refused destination must not become an offered move")
}

// countTaskTransitionAudits counts task.transition rows carrying one outcome. It
// reads through the founder because only a founder may read the organization-wide
// trail. Callers compare successive counts, so pre-existing rows cancel out.
func countTaskTransitionAudits(t *testing.T, token, outcome string) int {
	t.Helper()
	status, body := authJSONRaw(t, http.MethodGet,
		"/audit/events?event_type=task.transition&outcome="+outcome+"&limit=200", "", token)
	require.Equal(t, http.StatusOK, status, "the audit read must succeed: %s", body)
	var page auditPage
	require.NoError(t, json.Unmarshal(body, &page))
	return len(page.Events)
}
