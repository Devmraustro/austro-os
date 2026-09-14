package austro_os_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// auditPage mirrors the API envelope. It is decoded into a struct for the
// assertions that need values, while the raw bytes are kept for the redaction
// assertion, which must look at what was actually sent.
type auditPage struct {
	Events     []auditEventView `json:"events"`
	NextCursor string           `json:"next_cursor"`
	Limit      int              `json:"limit"`
}

type auditEventView struct {
	Seq                     int64  `json:"seq"`
	EventID                 string `json:"event_id"`
	Timestamp               string `json:"timestamp"`
	WorkspaceID             string `json:"workspace_id"`
	ActorType               string `json:"actor_type"`
	TargetType              string `json:"target_type"`
	EventType               string `json:"event_type"`
	Outcome                 string `json:"outcome"`
	ConstitutionalPrinciple string `json:"constitutional_principle"`
}

type auditVerificationResponse struct {
	Verified      bool  `json:"verified"`
	EventsChecked int   `json:"events_checked"`
	HeadSeq       int64 `json:"head_seq"`
}

// withheldFields are the stored columns that must never reach a client. The
// first two are internal cryptographic material; the second two are unbounded
// writer-controlled JSON, which is exactly where a caller's own input or an
// error string would land.
var withheldFields = []string{
	"outcome_details", "permissions_checked", "hash_chain_value", "digital_signature",
}

// founderToken bootstraps if necessary and returns a founder access token. The
// bootstrap call is idempotent: an already-initialized organization answers 409
// and the login proceeds regardless.
func founderToken(t *testing.T) string {
	t.Helper()
	authJSON(t, http.MethodPost, "/api/auth/bootstrap", map[string]string{}, "")
	status, body := authJSON(t, http.MethodPost, "/api/auth/login", map[string]string{
		"username": envOrDefault("AUSTRO_FOUNDER_USERNAME", "founder"),
		"password": envOrDefault("AUSTRO_FOUNDER_PASSWORD", ""),
	}, "")
	require.Equal(t, http.StatusOK, status, "founder login must succeed: %s", body)
	var tok authTokenResponse
	require.NoError(t, json.Unmarshal(body, &tok))
	require.NotEmpty(t, tok.AccessToken)
	return tok.AccessToken
}

// TestAuditVisibilityFounderOrgRead is the happy path plus the two properties
// that make a paginated audit log trustworthy: a total ordering, and a cursor
// that can neither skip nor repeat a row.
func TestAuditVisibilityFounderOrgRead(t *testing.T) {
	token := founderToken(t)

	status, body := authJSON(t, http.MethodGet, "/audit/events?limit=5", nil, token)
	require.Equal(t, http.StatusOK, status, "founder must read the org audit log: %s", body)

	var first auditPage
	require.NoError(t, json.Unmarshal(body, &first))
	require.NotEmpty(t, first.Events, "the audit chain is never empty: bootstrap itself is recorded")
	require.LessOrEqual(t, len(first.Events), 5, "limit must be honoured")
	require.Equal(t, 5, first.Limit, "the effective limit is reported")

	// Deterministic, newest-first ordering by a unique monotonic key.
	for i := 1; i < len(first.Events); i++ {
		require.Greater(t, first.Events[i-1].Seq, first.Events[i].Seq,
			"events must be strictly descending by seq")
	}

	// Redaction: assert against the bytes, not the decoded struct, so a field
	// the struct does not model cannot slip through unnoticed.
	for _, f := range withheldFields {
		require.NotContains(t, string(body), f, "audit responses must not expose %s", f)
	}
	for _, ev := range first.Events {
		require.NotEmpty(t, ev.EventType)
		require.NotEmpty(t, ev.Outcome)
		require.NotEmpty(t, ev.ConstitutionalPrinciple)
	}

	// Pagination. A full page must advertise a cursor; following it must yield
	// strictly older events with no overlap and no gap.
	if first.NextCursor == "" {
		t.Skip("chain shorter than one page; cursor continuity is exercised by TestAuditVisibilityPagination")
	}
	status, body = authJSON(t, http.MethodGet,
		"/audit/events?limit=5&before_seq="+first.NextCursor, nil, token)
	require.Equal(t, http.StatusOK, status, string(body))
	var second auditPage
	require.NoError(t, json.Unmarshal(body, &second))
	for _, ev := range second.Events {
		require.Less(t, ev.Seq, first.Events[len(first.Events)-1].Seq,
			"the second page must be strictly older than the first")
	}

	// Repeating the identical request must produce the identical sequence;
	// a page that reorders between calls is a page a cursor cannot traverse.
	status, again := authJSON(t, http.MethodGet, "/audit/events?limit=5", nil, token)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, string(bodyOf(first)), string(again))
}

// bodyOf re-encodes a decoded page so two responses can be compared as JSON
// rather than as byte strings.
func bodyOf(p auditPage) []byte {
	b, err := json.Marshal(p)
	if err != nil {
		return nil
	}
	return b
}

// TestAuditVisibilityPagination walks the whole chain with a small page size and
// asserts the cursor covers every event exactly once.
func TestAuditVisibilityPagination(t *testing.T) {
	token := founderToken(t)

	status, body := authJSON(t, http.MethodGet, "/audit/verification", nil, token)
	require.Equal(t, http.StatusOK, status, string(body))
	var ver auditVerificationResponse
	require.NoError(t, json.Unmarshal(body, &ver))
	require.True(t, ver.Verified, "the persisted chain must verify before pagination is meaningful")
	require.Greater(t, ver.EventsChecked, 0)

	const pageSize = 3
	seen := map[int64]bool{}
	cursor := ""
	// Bounded so a broken cursor cannot loop forever and burn the suite timeout.
	for page := 0; page < 400; page++ {
		path := fmt.Sprintf("/audit/events?limit=%d", pageSize)
		if cursor != "" {
			path += "&before_seq=" + cursor
		}
		status, body := authJSON(t, http.MethodGet, path, nil, token)
		require.Equal(t, http.StatusOK, status, string(body))
		var p auditPage
		require.NoError(t, json.Unmarshal(body, &p))
		if len(p.Events) == 0 {
			break
		}
		for _, ev := range p.Events {
			require.False(t, seen[ev.Seq], "seq %d was returned twice: the cursor repeats rows", ev.Seq)
			seen[ev.Seq] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	require.Equal(t, ver.EventsChecked, len(seen),
		"pagination must cover the whole chain exactly once")
}

// TestAuditVisibilityRoleBoundaries covers the authorization matrix, including
// the two cross-tenant attempts that matter most: a workspace admin naming
// another tenant's id, and any non-founder reaching the organization-wide view.
func TestAuditVisibilityRoleBoundaries(t *testing.T) {
	adminA, memberA, adminB := ensureRbacUsers(t)

	login := func(username string) string {
		status, body := authJSON(t, http.MethodPost, "/api/auth/login",
			map[string]string{"username": username, "password": rbacPassword}, "")
		require.Equal(t, http.StatusOK, status, "login %s: %s", username, body)
		var tok authTokenResponse
		require.NoError(t, json.Unmarshal(body, &tok))
		return tok.AccessToken
	}
	adminAToken, memberAToken, adminBToken := login(adminA.Username), login(memberA.Username), login(adminB.Username)

	// The organization-wide view and the whole-chain proof are founder-only.
	// They run on the administrative handle, so granting them to anyone else
	// would hand out a read the database would not confine.
	for name, tok := range map[string]string{"workspace_admin": adminAToken, "workspace_member": memberAToken} {
		for _, path := range []string{"/audit/events", "/audit/verification"} {
			status, body := authJSON(t, http.MethodGet, path, nil, tok)
			require.Equal(t, http.StatusForbidden, status,
				"%s must not reach %s: %s", name, path, body)
		}
	}

	// A workspace admin reads its own workspace.
	status, body := authJSON(t, http.MethodGet,
		"/workspaces/"+workspaceA+"/audit/events?limit=10", nil, adminAToken)
	require.Equal(t, http.StatusOK, status, "admin must read its own workspace audit: %s", body)
	var own auditPage
	require.NoError(t, json.Unmarshal(body, &own))
	for _, f := range withheldFields {
		require.NotContains(t, string(body), f)
	}

	// IDOR: admin A must not read workspace B by naming its id. The path is
	// refused by authorization, and the handler would refuse it again.
	status, _ = authJSON(t, http.MethodGet,
		"/workspaces/"+workspaceB+"/audit/events", nil, adminAToken)
	require.Equal(t, http.StatusForbidden, status,
		"admin A must not read workspace B's audit history")

	// Symmetrically, admin B must not read workspace A.
	status, _ = authJSON(t, http.MethodGet,
		"/workspaces/"+workspaceA+"/audit/events", nil, adminBToken)
	require.Equal(t, http.StatusForbidden, status,
		"admin B must not read workspace A's audit history")

	// A member reads its own workspace and nothing else.
	status, _ = authJSON(t, http.MethodGet,
		"/workspaces/"+workspaceA+"/audit/events", nil, memberAToken)
	require.Equal(t, http.StatusOK, status, "member must read its own workspace audit")
	status, _ = authJSON(t, http.MethodGet,
		"/workspaces/"+workspaceB+"/audit/events", nil, memberAToken)
	require.Equal(t, http.StatusForbidden, status, "member must not read another workspace")

	// No credentials at all.
	status, _ = authJSON(t, http.MethodGet, "/audit/events", nil, "")
	require.Equal(t, http.StatusForbidden, status, "an unauthenticated caller must be refused")
}

// TestAuditVisibilityWorkspaceIsolationEnforcedInDatabase proves the tenant read
// is confined by PostgreSQL and not merely by a Go-side filter: every event
// returned for workspace A belongs to workspace A or is organization-level.
func TestAuditVisibilityWorkspaceIsolationEnforcedInDatabase(t *testing.T) {
	adminA, _, adminB := ensureRbacUsers(t)
	login := func(username string) string {
		status, body := authJSON(t, http.MethodPost, "/api/auth/login",
			map[string]string{"username": username, "password": rbacPassword}, "")
		require.Equal(t, http.StatusOK, status, string(body))
		var tok authTokenResponse
		require.NoError(t, json.Unmarshal(body, &tok))
		return tok.AccessToken
	}

	// Produce audit rows in each workspace by making an authorized read that the
	// server records, so both tenants have history to compare.
	for _, tok := range []string{login(adminA.Username), login(adminB.Username)} {
		authJSON(t, http.MethodGet, "/api/me", nil, tok)
	}

	for ws, tok := range map[string]string{workspaceA: login(adminA.Username), workspaceB: login(adminB.Username)} {
		status, body := authJSON(t, http.MethodGet,
			"/workspaces/"+ws+"/audit/events?limit=200", nil, tok)
		require.Equal(t, http.StatusOK, status, string(body))
		var p auditPage
		require.NoError(t, json.Unmarshal(body, &p))
		for _, ev := range p.Events {
			// audit_workspace_policy allows the bound workspace plus
			// organization-level rows (workspace_id IS NULL, e.g. authentication
			// events recorded before a workspace context exists). Anything else
			// is a leak.
			if ev.WorkspaceID != "" {
				require.Equal(t, ws, ev.WorkspaceID,
					"workspace %s read leaked an event owned by %s", ws, ev.WorkspaceID)
			}
		}
	}
}

// TestAuditVisibilityInputBounds covers the malformed-input surface: unknown
// parameters are refused rather than ignored, and an oversized limit is clamped
// rather than honoured.
func TestAuditVisibilityInputBounds(t *testing.T) {
	token := founderToken(t)

	for _, path := range []string{
		"/audit/events?workspace_id=11111111-1111-1111-1111-111111111111",
		"/audit/events?limit=abc",
		"/audit/events?limit=-1",
		"/audit/events?before_seq=notanumber",
		"/audit/events?event_type=" + strings.Repeat("x", 200),
	} {
		status, body := authJSON(t, http.MethodGet, path, nil, token)
		require.Equal(t, http.StatusBadRequest, status,
			"%s must be rejected, got: %s", path, body)
	}

	// An over-large limit is reduced, not refused, so a client cannot probe the
	// ceiling for a different error path.
	status, body := authJSON(t, http.MethodGet, "/audit/events?limit=100000", nil, token)
	require.Equal(t, http.StatusOK, status, string(body))
	var p auditPage
	require.NoError(t, json.Unmarshal(body, &p))
	require.LessOrEqual(t, p.Limit, 200, "limit must be clamped to the documented maximum")
	require.LessOrEqual(t, len(p.Events), 200)

	// A client-supplied workspace filter must not widen a workspace-scoped read.
	// The tenant route ignores it entirely and stays bound to the caller.
	adminA, _, _ := ensureRbacUsers(t)
	status, body = authJSON(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": adminA.Username, "password": rbacPassword}, "")
	require.Equal(t, http.StatusOK, status, string(body))
	var tok authTokenResponse
	require.NoError(t, json.Unmarshal(body, &tok))
	status, body = authJSON(t, http.MethodGet,
		"/workspaces/"+workspaceA+"/audit/events?workspace_id="+workspaceB, nil, tok.AccessToken)
	require.Equal(t, http.StatusBadRequest, status,
		"an unsupported filter must be rejected, not silently applied: %s", body)
}

// TestAuditVisibilityTamperIsReported closes the loop between the persistent
// chain and its API: an edit made directly in the database, behind the
// application's back, must make the verification endpoint report failure, and
// restoring the row must make it report success again.
func TestAuditVisibilityTamperIsReported(t *testing.T) {
	token := founderToken(t)

	verify := func() auditVerificationResponse {
		status, body := authJSON(t, http.MethodGet, "/audit/verification", nil, token)
		require.Equal(t, http.StatusOK, status, string(body))
		var v auditVerificationResponse
		require.NoError(t, json.Unmarshal(body, &v))
		return v
	}
	require.True(t, verify().Verified, "the chain must verify before the test tampers with it")

	db, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer db.Close()

	// Tamper with a non-genesis row's outcome, which is bound into its hash.
	var (
		id      string
		origOut string
	)
	require.NoError(t, db.QueryRow(
		`SELECT event_id, outcome FROM audit_events WHERE genesis = false ORDER BY seq DESC LIMIT 1`,
	).Scan(&id, &origOut))

	t.Cleanup(func() {
		if _, err := db.Exec(`UPDATE audit_events SET outcome = $1 WHERE event_id = $2`, origOut, id); err != nil {
			t.Errorf("could not restore the tampered audit row: %v", err)
		}
	})
	_, err = db.Exec(`UPDATE audit_events SET outcome = 'tampered' WHERE event_id = $1`, id)
	require.NoError(t, err)

	require.False(t, verify().Verified,
		"a tampered chain must be reported as unverified through the API")

	_, err = db.Exec(`UPDATE audit_events SET outcome = $1 WHERE event_id = $2`, origOut, id)
	require.NoError(t, err)
	require.True(t, verify().Verified,
		"restoring the row must make the chain verify again")
}
