package austro_os_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Phase 2 live-database step used to assert a bare count of policies named
// workspace_isolation_policy. That count stayed at 10 after six execution
// tables gained the policy, so the first CI run after publication failed
// against a database that was correct:
//
//	ERROR: workspace_isolation_policy not fully provisioned (expected 10)
//
// A count is the wrong shape of assertion. It cannot say which table is wrong,
// it cannot distinguish a missing policy from a stale expectation, and it has
// to be edited every time a table is added. This test pins the workflow's
// explicit table list to the tables infrastructure/database actually
// provisions, in both directions, so drift is caught offline instead of by a
// red live job. It is a static contract between two source files; the live
// check is the CI step itself.

// createPolicyOnTable matches the SQL the bootstrap executes, including the
// EXECUTE '...' form inside the provisioning DO block.
var createPolicyOnTable = regexp.MustCompile(`CREATE POLICY ([a-z_]+) ON ([a-z_]+)`)

// policyCoverageMarker anchors the workflow extraction. The step must keep
// stating the table list explicitly; if that is removed or restructured, this
// test fails rather than silently reading an unrelated list.
const policyCoverageMarker = "-- workspace_isolation_policy coverage:"

// tablesWithPolicy returns the sorted, de-duplicated tables on which src creates
// the named policy.
func tablesWithPolicy(t *testing.T, src, policy string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range createPolicyOnTable.FindAllStringSubmatch(src, -1) {
		if m[1] == policy {
			seen[m[2]] = true
		}
	}
	tables := make([]string, 0, len(seen))
	for table := range seen {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

// assertedPolicyTables extracts the explicit VALUES list from the Phase 2
// workflow's workspace isolation coverage check.
func assertedPolicyTables(t *testing.T, workflow string) []string {
	t.Helper()

	start := strings.Index(workflow, policyCoverageMarker)
	require.GreaterOrEqual(t, start, 0,
		"the Phase 2 step must state the expected workspace_isolation_policy tables explicitly (marker %q not found)",
		policyCoverageMarker)

	rest := workflow[start:]
	end := strings.Index(rest, "AS v(tbl)")
	require.GreaterOrEqual(t, end, 0,
		"the workspace isolation coverage list must end with AS v(tbl)")

	row := regexp.MustCompile(`\('([a-z_]+)'\)`)
	seen := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(rest[:end], -1) {
		seen[m[1]] = true
	}
	tables := make([]string, 0, len(seen))
	for table := range seen {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

func TestPhase2RLSPolicyAssertionMatchesProvisioning(t *testing.T) {
	root := repoRootForIdentity(t)
	workflow := readFile(t, root, ".github/workflows/phase2-ci.yml")
	provisioning := readFile(t, root, "infrastructure/database/database.go")

	provisioned := tablesWithPolicy(t, provisioning, "workspace_isolation_policy")
	asserted := assertedPolicyTables(t, workflow)

	require.NotEmpty(t, provisioned, "setupRLSPolicies must provision workspace_isolation_policy")
	require.NotEmpty(t, asserted, "the Phase 2 step must assert workspace_isolation_policy coverage")

	require.ElementsMatch(t, provisioned, asserted,
		"the Phase 2 assertion must name exactly the tables setupRLSPolicies provisions; provisioned=%v asserted=%v",
		provisioned, asserted)

	// The bare count is what drifted. Keep it gone, so a future edit cannot
	// quietly reintroduce an expectation that has to be manually recounted.
	require.NotContains(t, workflow, "workspace_isolation_policy') <> 10",
		"the stale bare-count assertion must not return")
}

// users and audit_events are deliberately outside the workspace-policy set:
// they carry their own policies, created in the same provisioning block.
// If the 16-table list absorbed them the count would still look "complete"
// while neither table had the policy that actually protects it.
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
}
