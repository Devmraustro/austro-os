package austro_os_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"austro-os/infrastructure/auditstore"
	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/config"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
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

// Shared sessions.
//
// Both credential routes are rate limited: 5 bootstraps and 10 logins per
// minute per client address, and the whole suite issues its requests from one
// address inside a window of a few seconds. A helper that authenticated per
// test therefore exhausted the login budget partway through the run, and every
// later test that needed a session failed on 429 -- which read like a product
// regression but was the limiter working exactly as designed. So the founder
// session is established once for the package, and the workspace identities are
// minted rather than logged in, which is the pattern rbac_authorization_test.go
// already uses.
//
// The bodies below the sync.Once are deliberately free of *testing.T. They used
// to call t-dependent helpers, so a Fatal inside the once body unwound the
// goroutine with runtime.Goexit while sync.Once -- which marks itself done from
// a defer -- recorded success. The error variable was never assigned, so
// require.NoError saw nil and passed, and every later caller in the package
// failed on `Should NOT be empty` against a token that no test ever received.
// An initializer now returns (value, error); requireSharedToken is the single
// place a failure is surfaced, so each caller reports the real cause.
var (
	auditFounderOnce sync.Once
	auditFounderTok  string
	auditFounderErr  error

	auditRBACOnce sync.Once
	auditRBACTok  rbacSessions
	auditRBACErr  error
)

// rbacSessions holds one access token per workspace identity these tests use:
// an admin and a member of workspace A, and an admin of workspace B.
type rbacSessions struct {
	adminA  string
	memberA string
	adminB  string
}

// auditFounderToken returns a founder access token, performing at most one
// bootstrap and one login for the whole package. Bootstrap is idempotent: an
// already-initialized organization answers 409 and the login proceeds anyway.
func auditFounderToken(t *testing.T) string {
	t.Helper()
	auditFounderOnce.Do(func() {
		auditFounderTok, auditFounderErr = initAuditFounderSession(
			getEnv().apiURL,
			envOrDefault("AUSTRO_FOUNDER_USERNAME", "founder"),
			envOrDefault("AUSTRO_FOUNDER_PASSWORD", ""),
		)
	})
	requireSharedToken(t, "founder", auditFounderErr, map[string]string{
		"adminA": auditFounderTok,
	})
	return auditFounderTok
}

// initAuditFounderSession performs the one bootstrap and login and reports every
// failure as an error. It never asserts and never returns an empty token without
// also returning an error, so a cached failure cannot masquerade as a session.
func initAuditFounderSession(apiBase, username, password string) (string, error) {
	status, body, err := callAuthJSON(apiBase, http.MethodPost, "/api/auth/bootstrap", map[string]string{}, "")
	if err != nil {
		return "", fmt.Errorf("bootstrapping the founder session: %w", err)
	}
	if status >= http.StatusInternalServerError {
		return "", fmt.Errorf("bootstrap failed (%d): %s", status, body)
	}

	status, body, err = callAuthJSON(apiBase, http.MethodPost, "/api/auth/login", map[string]string{
		"username": username,
		"password": password,
	}, "")
	if err != nil {
		return "", fmt.Errorf("logging the founder in: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("founder login failed (%d): %s", status, body)
	}

	var tok authTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("decoding the founder login response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("founder login returned no access_token: %s", body)
	}
	return tok.AccessToken, nil
}

// auditWorkspaceTokens returns one token per workspace identity. They are
// minted with the same signing secret the API is configured with -- compose
// gives the test service the identical AUSTRO_JWT_SECRET -- so the server
// accepts them as ordinary access tokens, while the login rate limit is left
// intact for the tests that exercise the login route itself.
func auditWorkspaceTokens(t *testing.T) rbacSessions {
	t.Helper()
	auditRBACOnce.Do(func() {
		auditRBACTok, auditRBACErr = initAuditWorkspaceTokens(getEnv().postgresDSN)
	})
	requireSharedToken(t, "workspace", auditRBACErr, map[string]string{
		"adminA":  auditRBACTok.adminA,
		"memberA": auditRBACTok.memberA,
		"adminB":  auditRBACTok.adminB,
	})
	return auditRBACTok
}

// initAuditWorkspaceTokens provisions the three workspace identities and mints
// one access token each, returning errors instead of asserting. Like
// initAuditFounderSession it cannot leave an empty token without an error.
func initAuditWorkspaceTokens(dsn string) (rbacSessions, error) {
	adminA, memberA, adminB, err := provisionRbacUsers(dsn)
	if err != nil {
		return rbacSessions{}, err
	}
	svc := rbacJWT(&config.Config{
		JWTSecret:        envOrDefault("AUSTRO_JWT_SECRET", "change-me-in-production"),
		JWTRefreshSecret: envOrDefault("AUSTRO_JWT_REFRESH_SECRET", "change-me-in-production"),
	})
	mint := func(u *auth.UserRecord) (string, error) {
		role := auth.RoleFor(u)
		tok, err := svc.GenerateAccessTokenWithPermissions(
			u.ID, u.WorkspaceID, role, rbac.PermissionsForRole(role))
		if err != nil {
			return "", fmt.Errorf("minting a token for %s: %w", u.Username, err)
		}
		if tok == "" {
			return "", fmt.Errorf("minting a token for %s produced an empty token", u.Username)
		}
		return tok, nil
	}

	var out rbacSessions
	if out.adminA, err = mint(adminA); err != nil {
		return rbacSessions{}, err
	}
	if out.memberA, err = mint(memberA); err != nil {
		return rbacSessions{}, err
	}
	if out.adminB, err = mint(adminB); err != nil {
		return rbacSessions{}, err
	}
	return out, nil
}

// requireSharedToken is the only place a shared-session failure is surfaced.
// It reports the initializer's error, so an unreachable service or a rejected
// credential is named instead of showing up later as an empty token.
func requireSharedToken(t *testing.T, kind string, err error, tokens map[string]string) {
	t.Helper()
	if msg := sharedTokenFailure(kind, err, tokens); msg != "" {
		t.Fatal(msg)
	}
}

// sharedTokenFailure returns the diagnostic for a shared session that could not
// be handed to a caller, or "" when the session is usable. It is separate from
// requireSharedToken so the wording is directly testable: the original defect
// hid the real cause behind "Should NOT be empty", so the message is now part
// of the contract under test rather than an incidental string.
func sharedTokenFailure(kind string, err error, tokens map[string]string) string {
	if err != nil {
		return fmt.Sprintf("shared %s session could not be established: %v", kind, err)
	}
	// An empty token with no error should be unreachable: the initializers
	// refuse to return one. The check stays so that a future initializer
	// cannot reintroduce the silent-empty-token failure mode, and it is
	// worded as a harness fault because that is what it would be.
	names := make([]string, 0, len(tokens))
	for name, tok := range tokens {
		if tok == "" {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return fmt.Sprintf(
			"shared %s session initializer returned empty tokens (%s) with no error: "+
				"this is a test-harness fault, not a product failure",
			kind, strings.Join(names, ", "))
	}
	return ""
}

// TestAuditVisibilityFounderOrgRead is the happy path plus the two properties
// that make a paginated audit log trustworthy: a total ordering, and a cursor
// that can neither skip nor repeat a row.
func TestAuditVisibilityFounderOrgRead(t *testing.T) {
	token := auditFounderToken(t)

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
	//
	// The comparison is made on decoded pages, not on re-encoded JSON. This
	// struct models a subset of the wire fields, so marshalling it back would
	// drop the fields it does not declare and could never equal the raw
	// response -- the assertion would fail for a reason that has nothing to do
	// with ordering.
	status, again := authJSON(t, http.MethodGet, "/audit/events?limit=5", nil, token)
	require.Equal(t, http.StatusOK, status)
	var repeat auditPage
	require.NoError(t, json.Unmarshal(again, &repeat))
	require.Equal(t, first, repeat,
		"repeating the identical request must return the identical page")
}

// TestAuditVisibilityPagination walks the whole chain with a small page size and
// asserts the cursor covers every event exactly once.
func TestAuditVisibilityPagination(t *testing.T) {
	token := auditFounderToken(t)

	status, body := authJSON(t, http.MethodGet, "/audit/verification", nil, token)
	require.Equal(t, http.StatusOK, status, string(body))
	var ver auditVerificationResponse
	require.NoError(t, json.Unmarshal(body, &ver))
	require.True(t, ver.Verified, "the persisted chain must verify before pagination is meaningful")
	require.Greater(t, ver.EventsChecked, 0)

	const pageSize = 3
	// The chain keeps growing as later tests (and repeated runs) append events,
	// so the guard is derived from the live chain length instead of a fixed
	// page budget: a page limit that is only comfortably large for a short
	// chain would silently stop the walk early and look like a skipped row.
	maxPages := (ver.EventsChecked/pageSize + 2) * 2
	seen := map[int64]bool{}
	cursor := ""
	// Bounded so a broken cursor cannot loop forever and burn the suite timeout.
	for page := 0; page < maxPages; page++ {
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
	sessions := auditWorkspaceTokens(t)
	adminAToken, memberAToken, adminBToken := sessions.adminA, sessions.memberA, sessions.adminB

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
	tokens := auditWorkspaceTokens(t)

	// Produce audit rows in each workspace by making an authorized read that the
	// server records, so both tenants have history to compare.
	for _, tk := range []string{tokens.adminA, tokens.adminB} {
		authJSON(t, http.MethodGet, "/api/me", nil, tk)
	}

	for ws, tok := range map[string]string{workspaceA: tokens.adminA, workspaceB: tokens.adminB} {
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
	token := auditFounderToken(t)

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
	// The tenant route refuses the unsupported parameter rather than silently
	// applying a scope the caller did not earn.
	status, body = authJSON(t, http.MethodGet,
		"/workspaces/"+workspaceA+"/audit/events?workspace_id="+workspaceB,
		nil, auditWorkspaceTokens(t).adminA)
	require.Equal(t, http.StatusBadRequest, status,
		"an unsupported filter must be rejected, not silently applied: %s", body)
}

// TestAuditVisibilityTamperIsReported closes the loop between the persistent
// chain and its API: an edit made directly in the database, behind the
// application's back, must make the verification endpoint report failure, and
// restoring the row must make it report success again.
func TestAuditVisibilityTamperIsReported(t *testing.T) {
	token := auditFounderToken(t)

	verify := func() auditVerificationResponse {
		status, body := authJSON(t, http.MethodGet, "/audit/verification", nil, token)
		require.Equal(t, http.StatusOK, status, string(body))
		var v auditVerificationResponse
		require.NoError(t, json.Unmarshal(body, &v))
		return v
	}

	// Connect with a handle that outlives the test body. t.Cleanup runs after
	// the deferred Close of anything opened here, so a cleanup that reused a
	// closed handle would silently fail to restore the row and leave the chain
	// broken for every test that runs afterwards.
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	// Tamper with a row this test owns rather than whichever event happens to
	// be newest. Picking the newest row means editing another test's event, and
	// it makes the outcome depend on what else the suite recorded.
	const probeType = "austro.tamper_probe"
	probeMarker := "tamper-" + uuid.NewString()[:8]

	store, err := auditstore.New(context.Background(), admin)
	require.NoError(t, err)
	_, err = store.Append(context.Background(), audit.Record{
		EventType:  probeType,
		ActorType:  "user",
		TargetType: probeMarker,
		Outcome:    "success",
		Principle:  "Security by Design",
	})
	require.NoError(t, err, "the probe event must be persisted before it is tampered with")

	var probeID string
	require.NoError(t, admin.QueryRow(
		`SELECT event_id FROM audit_events WHERE target_type = $1`, probeMarker,
	).Scan(&probeID))

	// Restore unconditionally, on a connection opened inside the cleanup, and
	// fail loudly if the database could not be put back as it was found. A
	// tamper test that can leave the chain broken is worse than no test.
	t.Cleanup(func() {
		db, err := getEnv().AdminDB()
		if err != nil {
			t.Errorf("could not reconnect to restore the tampered audit row: %v", err)
			return
		}
		defer db.Close()
		if _, err := db.Exec(
			`UPDATE audit_events SET outcome = 'success' WHERE event_id = $1`, probeID); err != nil {
			t.Errorf("could not restore the tampered audit row %s: %v", probeID, err)
			return
		}
		if !verify().Verified {
			t.Errorf("the audit chain does not verify after restoring %s", probeID)
		}
	})

	// Record the state before tampering instead of assuming the chain is clean:
	// what this test proves is the transition, not the ambient state.
	before := verify()
	require.True(t, before.Verified,
		"the chain must verify before this test tampers with it (%d events checked)",
		before.EventsChecked)

	_, err = admin.Exec(`UPDATE audit_events SET outcome = 'tampered' WHERE event_id = $1`, probeID)
	require.NoError(t, err, "the owner role must be able to edit the table; the append-only guarantee is a privilege grant, not a trigger")

	after := verify()
	require.False(t, after.Verified,
		"an edit made behind the application's back must be reported as unverified")
	require.Equal(t, before.EventsChecked, after.EventsChecked,
		"verification must still cover the whole chain when it reports a break")

	_, err = admin.Exec(`UPDATE audit_events SET outcome = 'success' WHERE event_id = $1`, probeID)
	require.NoError(t, err)
	require.True(t, verify().Verified,
		"restoring the row must make the chain verify again")
}
