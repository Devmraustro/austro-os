package austro_os_test

import (
	"context"
	"database/sql"
	"testing"

	"austro-os/infrastructure/auditstore"
	"austro-os/internal/audit"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests drive the audit read layer directly against a live PostgreSQL,
// which is the part the HTTP tests cannot isolate: the SQL, the cursor, the
// filter bounds, and above all which principal each read runs as. The API tests
// in audit_visibility_live_test.go cover the same surface end to end.

// auditReadFixture appends a small, identifiable set of events to the chain so the
// read tests have something deterministic to assert against. Events are never
// deleted -- the table is append-only -- so assertions count deltas and filter
// by the unique marker rather than by absolute position.
func auditReadFixture(t *testing.T, wsA, wsB uuid.UUID, marker string) *auditstore.Store {
	t.Helper()
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })

	store, err := auditstore.New(context.Background(), admin)
	require.NoError(t, err)

	append := func(ws *uuid.UUID, outcome string) {
		_, err := store.Append(context.Background(), audit.Record{
			EventType:   "auditread." + marker,
			ActorType:   "user",
			TargetType:  "audit_read_test",
			Outcome:     outcome,
			Principle:   "Security by Design",
			WorkspaceID: ws,
			Details: map[string]any{
				"marker": marker,
				// Deliberately sensitive-looking: the read model must not
				// expose outcome_details, so this value must never appear in
				// an API response.
				"secret": "must-never-be-serialized",
			},
		})
		require.NoError(t, err)
	}
	append(&wsA, "success")
	append(&wsA, "denied")
	append(&wsB, "success")
	append(nil, "success") // organization-level, visible to every workspace read
	return store
}

func TestAuditReaderListIsBoundedAndOrdered(t *testing.T) {
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	marker := uuid.NewString()[:8]
	auditReadFixture(t, wsA, wsB, marker)

	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	reader := auditstore.NewReader(admin)
	ctx := context.Background()

	page, err := reader.List(ctx, auditstore.Query{
		EventType: "auditread." + marker,
		Limit:     2,
	})
	require.NoError(t, err)
	require.Len(t, page, 2, "the limit must be honoured exactly")

	require.Greater(t, page[0].Seq, page[1].Seq, "ordering must be strictly descending by seq")

	// The cursor yields the remainder with no overlap.
	rest, err := reader.List(ctx, auditstore.Query{
		EventType: "auditread." + marker,
		Limit:     10,
		BeforeSeq: page[1].Seq,
	})
	require.NoError(t, err)
	require.Len(t, rest, 2, "four events were appended; two came back on the first page")
	for _, ev := range rest {
		require.Less(t, ev.Seq, page[1].Seq, "the cursor must return only older events")
	}

	seen := map[int64]bool{}
	for _, ev := range concatViews(page, rest) {
		require.False(t, seen[ev.Seq], "seq %d appeared on both pages", ev.Seq)
		seen[ev.Seq] = true
	}
	require.Len(t, seen, 4)
}

func concatViews(a, b []auditstore.EventView) []auditstore.EventView {
	return append(append([]auditstore.EventView{}, a...), b...)
}

func TestAuditReaderFiltersAreExactAndBounded(t *testing.T) {
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	marker := uuid.NewString()[:8]
	auditReadFixture(t, wsA, wsB, marker)

	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	reader := auditstore.NewReader(admin)
	ctx := context.Background()
	eventType := "auditread." + marker

	byOutcome, err := reader.List(ctx, auditstore.Query{
		EventType: eventType, Outcome: "denied", Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, byOutcome, 1, "exactly one denied event was appended")
	for _, ev := range byOutcome {
		require.Equal(t, "denied", ev.Outcome)
		require.Equal(t, eventType, ev.EventType)
	}

	// A filter is an exact match, so a prefix of a real value matches nothing.
	// Silently treating it as a prefix would return rows the caller did not ask
	// for, which is a wrong answer rather than a refused one.
	partial, err := reader.List(ctx, auditstore.Query{
		EventType: "auditread." + marker[:4], Limit: 50,
	})
	require.NoError(t, err)
	require.Empty(t, partial, "filters must be exact matches, not prefixes")

	// An over-long filter is rejected rather than truncated: truncating would
	// match different rows than the caller asked for.
	long := auditstore.Query{Limit: 10, EventType: repeat("x", 200)}
	_, err = reader.List(ctx, long)
	require.Error(t, err, "an over-long filter must be refused")

	// The limit is clamped, not rejected: a caller asking for too much gets the
	// documented maximum.
	clamped := auditstore.Query{Limit: 100000}
	require.NoError(t, clamped.Normalize())
	require.Equal(t, auditstore.MaxPageSize, clamped.Limit)
	zero := auditstore.Query{}
	require.NoError(t, zero.Normalize())
	require.Equal(t, auditstore.DefaultPageSize, zero.Limit)
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// TestAuditReaderWorkspaceScopeIsEnforcedByDatabase is the isolation proof for
// the read path. It runs on the unprivileged runtime handle, so
// audit_workspace_policy -- not Go -- decides what is visible.
func TestAuditReaderWorkspaceScopeIsEnforcedByDatabase(t *testing.T) {
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	marker := uuid.NewString()[:8]
	auditReadFixture(t, wsA, wsB, marker)

	runtimeDSN := envOrDefault("AUSTRO_POSTGRES_RUNTIME_DSN", "")
	require.NotEmpty(t, runtimeDSN,
		"the runtime DSN is required: this test must not run on a privileged connection")
	// Connect with the runtime DSN verbatim. connectAs() rewrites the user, and
	// the whole point here is to be the unprivileged role the API actually uses.
	ctx := context.Background()
	runtime, err := sql.Open("pgx", runtimeDSN)
	require.NoError(t, err)
	defer runtime.Close()
	require.NoError(t, runtime.PingContext(ctx),
		"the runtime role must be able to connect")
	reader := auditstore.NewReader(runtime)
	eventType := "auditread." + marker

	got, err := reader.ListForWorkspace(ctx, wsA, auditstore.Query{EventType: eventType, Limit: 50})
	require.NoError(t, err)
	require.NotEmpty(t, got)
	for _, ev := range got {
		if ev.Workspace != nil {
			require.Equal(t, wsA, *ev.Workspace,
				"a workspace A read returned an event owned by %s", ev.Workspace)
		}
	}

	// Symmetrically, a workspace-B read must not contain workspace A's events.
	// This is the assertion that would fail if isolation were only a Go-side
	// filter, because the reader here is the unprivileged runtime role and
	// audit_workspace_policy is what confines it.
	gotB, err := reader.ListForWorkspace(ctx, wsB, auditstore.Query{EventType: eventType, Limit: 50})
	require.NoError(t, err)
	require.NotEmpty(t, gotB, "workspace B has events of its own")
	for _, ev := range gotB {
		if ev.Workspace != nil {
			require.Equal(t, wsB, *ev.Workspace,
				"a workspace B read returned an event owned by %s", ev.Workspace)
		}
	}

	// Asking for workspace B's events through a workspace-A read must not return
	// workspace B. ListForWorkspace forces the scope from its argument rather
	// than honouring a caller-supplied filter, so the read stays on workspace A
	// -- and RLS would hide B's rows even if it did not. Either way the caller
	// cannot widen a tenant-scoped read; what matters is that nothing belonging
	// to B comes back.
	q := auditstore.Query{EventType: eventType, Limit: 50}
	other := wsB
	q.WorkspaceID = &other
	sneaky, err := reader.ListForWorkspace(ctx, wsA, q)
	require.NoError(t, err)
	require.NotEmpty(t, sneaky, "the forced workspace-A scope still has events")
	for _, ev := range sneaky {
		require.NotEqual(t, wsB, ev.Workspace,
			"a workspace-scoped read leaked a row owned by the workspace named in the filter")
		if ev.Workspace != nil {
			require.Equal(t, wsA, *ev.Workspace)
		}
	}
}

// TestAuditReaderRedactsSensitiveColumns asserts the projection at the source:
// the read model has no field for the columns that must not leave the database.
func TestAuditReaderRedactsSensitiveColumns(t *testing.T) {
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	marker := uuid.NewString()[:8]
	auditReadFixture(t, wsA, wsB, marker)

	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	reader := auditstore.NewReader(admin)

	page, err := reader.List(context.Background(), auditstore.Query{
		EventType: "auditread." + marker, Limit: 10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, page)
	for _, ev := range page {
		require.NotEmpty(t, ev.EventType)
		require.NotEmpty(t, ev.Outcome)
		require.NotEmpty(t, ev.ConstitutionalPrinciple)
		require.NotZero(t, ev.EventID)
	}
	// The fixture wrote a secret-looking value into outcome_details. The view
	// type cannot carry it, which is the structural guarantee; the HTTP test
	// asserts the same thing against the serialized bytes.
}

// TestAuditReaderVerifyChainMatchesTheWriter checks that the reader's
// independent verification agrees with the writer's.
func TestAuditReaderVerifyChainMatchesTheWriter(t *testing.T) {
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	marker := uuid.NewString()[:8]
	store := auditReadFixture(t, wsA, wsB, marker)

	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	reader := auditstore.NewReader(admin)
	ctx := context.Background()

	ok, count, err := reader.VerifyChain(ctx)
	require.NoError(t, err)
	require.True(t, ok, "the persisted chain must verify")
	require.Greater(t, count, 0)

	writerOK, err := store.Verify(ctx)
	require.NoError(t, err)
	require.Equal(t, writerOK, ok,
		"the reader's verification must agree with the writer's")
}
