package austro_os_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Phase 2 live-database step used to assert bare counts: exactly 12 tables
// with row level security enabled and forced, exactly 10 policies named
// workspace_isolation_policy. Both drifted. The six execution tables
// (ai_executions, ai_execution_retries, retry_dispatch_log, ai_execution_dlq,
// ai_tool_calls, execution_contexts) were added to the schema and to the
// policies without updating any count, so the first CI run after publication
// failed against a database that was correct:
//
//	ERROR: workspace_isolation_policy not fully provisioned (expected 10)
//
// A count is the wrong shape of assertion. It cannot say which table is wrong,
// it cannot distinguish a missing control from a stale expectation, and it has
// to be edited every time a table is added. This file pins all three workflow
// table lists to the single source of truth, rlsTables(), in both directions,
// so drift is caught offline instead of by a red live job.
//
// Three lists, two sets:
//
//   - enabled and forced must cover rlsTables() exactly (18 tables).
//   - workspace_isolation_policy covers rlsTables() minus users and
//     audit_events (16 tables), because those two carry their own policies.
//
// It is a static contract between two source files. The live check is the CI
// step itself; this test cannot prove the SQL runs.

// createPolicyOnTable matches the SQL the bootstrap executes, including the
// EXECUTE '...' form inside the provisioning DO block.
var createPolicyOnTable = regexp.MustCompile(`CREATE POLICY ([a-z_]+) ON ([a-z_]+)`)

// Workflow markers anchor each extraction. Every Phase 2 check must keep
// stating its table list explicitly; if a marker is removed or the step is
// restructured, these tests fail rather than silently reading an unrelated
// list.
const (
	enabledCoverageMarker = "-- Row level security enabled coverage:"
	forcedCoverageMarker  = "-- Row level security forced coverage:"
	policyCoverageMarker  = "-- workspace_isolation_policy coverage:"
)

// rlsProtectedTables is the order-independent content of rlsTables(), the list
// both enableRLS() and forceRLS() iterate. Anything outside it is not
// RLS-protected by the bootstrap, so a workflow list that includes an outside
// table would assert a control the application does not maintain.
func rlsProtectedTables(t *testing.T, src string) []string {
	t.Helper()

	const header = "func rlsTables() []string {"
	start := strings.Index(src, header)
	require.GreaterOrEqual(t, start, 0,
		"infrastructure/database must declare rlsTables(); it is the source of truth for RLS coverage")

	rest := src[start:]
	lit := strings.Index(rest, "return []string{")
	require.GreaterOrEqual(t, lit, 0,
		"rlsTables() must return an explicit []string{...} literal so its contents can be verified")

	body := rest[lit:]
	end := strings.Index(body, "}")
	require.GreaterOrEqual(t, end, 0, "the rlsTables() literal must be closed")

	tables := []string{}
	for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(body[:end], -1) {
		tables = append(tables, m[1])
	}
	return sortedUnique(tables)
}

// assertedTablesAt extracts the explicit VALUES rows from the Phase 2 check
// identified by marker.
func assertedTablesAt(t *testing.T, workflow, marker string) []string {
	t.Helper()

	start := strings.Index(workflow, marker)
	require.GreaterOrEqual(t, start, 0,
		"the Phase 2 step must state its expected tables explicitly (marker %q not found)", marker)

	rest := workflow[start:]
	end := strings.Index(rest, "AS v(tbl)")
	require.GreaterOrEqual(t, end, 0,
		"the table list after %q must end with AS v(tbl)", marker)

	tables := []string{}
	for _, m := range regexp.MustCompile(`\('([a-z_]+)'\)`).FindAllStringSubmatch(rest[:end], -1) {
		tables = append(tables, m[1])
	}
	return sortedUnique(tables)
}

func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	for _, v := range values {
		seen[v] = true
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// tablesWithPolicy returns the sorted, de-duplicated tables on which src creates
// the named policy.
func tablesWithPolicy(t *testing.T, src, policy string) []string {
	t.Helper()
	tables := []string{}
	for _, m := range createPolicyOnTable.FindAllStringSubmatch(src, -1) {
		if m[1] == policy {
			tables = append(tables, m[2])
		}
	}
	return sortedUnique(tables)
}

// RLS being enabled is not enough: PostgreSQL exempts the table owner unless
// the security is forced, and the owner credential is the one the bootstrap
// uses. A table that is enabled but not forced is a silent isolation hole, so
// the workflow must assert both over the same complete set.
func TestPhase2RLSEnabledAndForcedCoverageMatchesProtectedTables(t *testing.T) {
	root := repoRootForIdentity(t)
	workflow := readFile(t, root, ".github/workflows/phase2-ci.yml")
	provisioning := readFile(t, root, "infrastructure/database/database.go")

	protected := rlsProtectedTables(t, provisioning)
	require.NotEmpty(t, protected, "rlsTables() must not be empty")

	enabled := assertedTablesAt(t, workflow, enabledCoverageMarker)
	forced := assertedTablesAt(t, workflow, forcedCoverageMarker)

	require.ElementsMatch(t, protected, enabled,
		"the RLS-enabled assertion must name exactly the tables rlsTables() protects; protected=%v enabled=%v",
		protected, enabled)
	require.ElementsMatch(t, protected, forced,
		"the RLS-forced assertion must name exactly the tables rlsTables() protects; protected=%v forced=%v",
		protected, forced)

	// The six execution tables are the ones the old 12-table check missed.
	// Naming them here keeps that regression legible if the list is ever
	// trimmed back to the pre-execution-table set.
	for _, table := range []string{
		"ai_executions",
		"ai_execution_retries",
		"retry_dispatch_log",
		"ai_execution_dlq",
		"ai_tool_calls",
		"execution_contexts",
	} {
		require.Contains(t, enabled, table,
			"%s must be covered by the RLS-enabled assertion", table)
		require.Contains(t, forced, table,
			"%s must be covered by the RLS-forced assertion", table)
	}

	// Counts are what drifted. Keep the bare-count form gone.
	require.NotContains(t, workflow, "relrowsecurity) <> 12",
		"the stale RLS-enabled count must not return")
	require.NotContains(t, workflow, "relforcerowsecurity) <> 12",
		"the stale RLS-forced count must not return")
}

func TestPhase2RLSPolicyAssertionMatchesProvisioning(t *testing.T) {
	root := repoRootForIdentity(t)
	workflow := readFile(t, root, ".github/workflows/phase2-ci.yml")
	provisioning := readFile(t, root, "infrastructure/database/database.go")

	provisioned := tablesWithPolicy(t, provisioning, "workspace_isolation_policy")
	asserted := assertedTablesAt(t, workflow, policyCoverageMarker)

	require.NotEmpty(t, provisioned, "setupRLSPolicies must provision workspace_isolation_policy")
	require.NotEmpty(t, asserted, "the Phase 2 step must assert workspace_isolation_policy coverage")

	require.ElementsMatch(t, provisioned, asserted,
		"the Phase 2 assertion must name exactly the tables setupRLSPolicies provisions; provisioned=%v asserted=%v",
		provisioned, asserted)

	// The workspace set is not the full protected set: users and audit_events
	// are protected by differently named policies. Deriving the expectation
	// from rlsTables() rather than from the provisioning source keeps the two
	// sets from being conflated.
	protected := rlsProtectedTables(t, provisioning)
	require.ElementsMatch(t, withoutUsersAndAuditEvents(protected), provisioned,
		"every RLS-protected table except users and audit_events must carry workspace_isolation_policy; protected=%v provisioned=%v",
		protected, provisioned)

	// The bare count is what drifted. Keep it gone, so a future edit cannot
	// quietly reintroduce an expectation that has to be manually recounted.
	require.NotContains(t, workflow, "workspace_isolation_policy') <> 10",
		"the stale bare-count assertion must not return")
}

func withoutUsersAndAuditEvents(tables []string) []string {
	out := make([]string, 0, len(tables))
	for _, table := range tables {
		if table == "users" || table == "audit_events" {
			continue
		}
		out = append(out, table)
	}
	sort.Strings(out)
	return out
}

// users and audit_events are deliberately outside the workspace-policy set:
// they carry their own policies, created in the same provisioning block. If the
// 16-table list absorbed them the job would still look "complete" while neither
// table had the policy that actually protects it. They are still in
// rlsTables(), so they must still be enabled and forced.
func TestDistinctPoliciesRemainForUsersAndAuditEvents(t *testing.T) {
	root := repoRootForIdentity(t)
	provisioning := readFile(t, root, "infrastructure/database/database.go")

	workspaceScoped := tablesWithPolicy(t, provisioning, "workspace_isolation_policy")
	require.NotContains(t, workspaceScoped, "users",
		"users is protected by user_scope_policy, not workspace_isolation_policy")
	require.NotContains(t, workspaceScoped, "audit_events",
		"audit_events is protected by audit_workspace_policy, not workspace_isolation_policy")

	require.Contains(t, tablesWithPolicy(t, provisioning, "user_scope_policy"), "users",
		"users must keep its identity-scoped policy")
	require.Contains(t, tablesWithPolicy(t, provisioning, "audit_workspace_policy"), "audit_events",
		"audit_events must keep its workspace-scoped policy")

	protected := rlsProtectedTables(t, provisioning)
	require.Contains(t, protected, "users",
		"users must still have row level security enabled and forced")
	require.Contains(t, protected, "audit_events",
		"audit_events must still have row level security enabled and forced")
}

// Two of the twenty application tables are outside rlsTables(). Excluding a
// table from RLS is a security decision, so it needs a reason in code and it
// must not be silently asserted as protected by CI. Both requirements are
// checked from the source that would drift.
//
//   - founders is organization-level, not tenant-level. Its policy is
//     USING (true), so row level security would evaluate to "all rows allowed"
//     and protect nothing.
//   - process_heartbeats is infrastructure liveness state. It has no workspace
//     owner and no tenant content, and the API must read the dispatcher's row
//     while serving any workspace.
func TestIntentionalRLSExclusionsAreDocumentedAndUnasserted(t *testing.T) {
	root := repoRootForIdentity(t)
	workflow := readFile(t, root, ".github/workflows/phase2-ci.yml")
	provisioning := readFile(t, root, "infrastructure/database/database.go")

	excluded := []string{"founders", "process_heartbeats"}

	protected := rlsProtectedTables(t, provisioning)
	for _, table := range excluded {
		require.NotContains(t, protected, table,
			"%s is intentionally outside rlsTables(); if that changed, the exclusion test and the docs must change with it",
			table)
	}

	// The workflow must not assert a control the bootstrap does not apply.
	for _, marker := range []string{enabledCoverageMarker, forcedCoverageMarker, policyCoverageMarker} {
		asserted := assertedTablesAt(t, workflow, marker)
		for _, table := range excluded {
			require.NotContains(t, asserted, table,
				"the Phase 2 check %q must not assert RLS on %s, which the bootstrap leaves unprotected",
				marker, table)
		}
	}

	// Evidence for founders: the org-wide policy is unconditional, so RLS
	// there is a no-op rather than a control.
	require.Regexp(t, regexp.MustCompile(`CREATE POLICY founder_org_policy ON founders\s+USING \(true\)`), provisioning,
		"founders must keep its documented USING (true) organization policy")

	// Evidence for process_heartbeats: the schema comment states why it holds
	// no tenant data. Losing that comment would leave the exclusion unexplained.
	const heartbeatTable = "CREATE TABLE IF NOT EXISTS process_heartbeats ("
	at := strings.Index(provisioning, heartbeatTable)
	require.GreaterOrEqual(t, at, 0, "the schema must still create process_heartbeats")
	preamble := provisioning[max0(at-1500):at]
	require.Contains(t, preamble, "Deliberately NOT row level security",
		"process_heartbeats must document that its RLS exclusion is deliberate")
	require.Contains(t, preamble, "no tenant content",
		"process_heartbeats must document that it holds no tenant content")
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
