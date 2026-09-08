package authz

import (
	"testing"

	"austro-os/internal/auth"
	"austro-os/internal/rbac"

	"github.com/stretchr/testify/require"
)

// claims builds a claims set with the given role, workspace and permissions.
func claims(role rbac.Role, workspace string, perms map[string][]string) *auth.Claims {
	return &auth.Claims{ID: "user-1", Role: role, WorkspaceID: workspace, Permissions: perms}
}

// profilePerms is the grant a member/admin/founder carries for /api/me.
func profilePerms() map[string][]string {
	return rbac.PermissionsForRole(rbac.RoleWorkspaceMember)
}

func seeded(t *testing.T, rules []rbac.Rule) *Authorizer {
	t.Helper()
	a := NewAuthorizer()
	a.AddRules(rules)
	return a
}

func TestAuthorizeMeAllRolesGranted(t *testing.T) {
	az := seeded(t, rbac.Rules())

	for _, role := range []rbac.Role{rbac.RoleFounder, rbac.RoleWorkspaceAdmin, rbac.RoleWorkspaceMember} {
		c := claims(role, workspaceFor(role), rbac.PermissionsForRole(role))
		require.NoError(t, az.AuthorizeClaims(c, "GET", "/api/me"),
			"role %s must reach its own profile", role)
	}
}

func TestAuthorizeUnauthenticatedDenied(t *testing.T) {
	az := seeded(t, rbac.Rules())
	err := az.AuthorizeClaims(nil, "GET", "/api/me")
	require.ErrorIs(t, err, ErrDenied)
}

func TestAuthorizeUnknownRouteDenied(t *testing.T) {
	az := seeded(t, rbac.Rules())
	c := claims(rbac.RoleFounder, "", rbac.PermissionsForRole(rbac.RoleFounder))
	// No rule (not even a template) matches this route -> denied, regardless of
	// how privileged the claims look.
	require.ErrorIs(t, az.AuthorizeClaims(c, "GET", "/api/nonexistent"), ErrDenied)
	require.ErrorIs(t, az.AuthorizeClaims(c, "GET", "/workspaces/unknown-uuid/resource"), ErrDenied)
}

func TestAuthorizeUnsupportedRoleDenied(t *testing.T) {
	az := seeded(t, rbac.Rules())
	c := claims(rbac.Role("superuser"), "", rbac.PermissionsForRole(rbac.RoleFounder))
	require.ErrorIs(t, az.AuthorizeClaims(c, "GET", "/api/me"), ErrDenied,
		"an unsupported role claim must never be granted")
}

func TestAuthorizeMissingPermissionsClaimDenied(t *testing.T) {
	az := seeded(t, rbac.Rules())
	c := claims(rbac.RoleWorkspaceMember, workspaceFor(rbac.RoleWorkspaceMember), nil)
	require.ErrorIs(t, az.AuthorizeClaims(c, "GET", "/api/me"), ErrDenied,
		"claims with no explicit permission for the action+route must be denied")
}

func TestWorkspaceAdminOwnWorkspaceAllowed(t *testing.T) {
	rules := adminWorkspaceRules()
	az := seeded(t, rules)
	admin := claims(rbac.RoleWorkspaceAdmin, workspaceA, rbac.PermissionsForRole(rbac.RoleWorkspaceAdmin))

	// Own workspace is allowed.
	require.NoError(t, az.AuthorizeClaims(admin, "GET", "/workspaces/"+workspaceA))
}

func TestWorkspaceCrossWorkspaceDenied(t *testing.T) {
	az := seeded(t, adminWorkspaceRules())
	admin := claims(rbac.RoleWorkspaceAdmin, workspaceA, rbac.PermissionsForRole(rbac.RoleWorkspaceAdmin))

	// A workspace-admin of A must not reach B by rewriting the path.
	require.ErrorIs(t, az.AuthorizeClaims(admin, "GET", "/workspaces/"+workspaceB), ErrDenied)
}

func TestLiveSurfaceWorkspaceRouteDenied(t *testing.T) {
	// The live authorizer seeds only the rules for routes actually registered
	// (ImplementedRules); the workspace administration surface is still only
	// declared (Rules), so even an admin carrying the grant is denied because
	// no rule is registered for it.
	az := seeded(t, rbac.ImplementedRules())
	admin := claims(rbac.RoleWorkspaceAdmin, workspaceA, rbac.PermissionsForRole(rbac.RoleWorkspaceAdmin))
	require.ErrorIs(t, az.AuthorizeClaims(admin, "GET", "/workspaces/"+workspaceA), ErrDenied)
}

func TestWorkspaceMemberRoleDenied(t *testing.T) {
	az := seeded(t, adminWorkspaceRules())
	member := claims(rbac.RoleWorkspaceMember, workspaceA, rbac.PermissionsForRole(rbac.RoleWorkspaceMember))
	require.ErrorIs(t, az.AuthorizeClaims(member, "GET", "/workspaces/"+workspaceA), ErrDenied,
		"a workspace member must be denied an administrative route")
}

func TestFounderOrgScopeExplicitNotBypass(t *testing.T) {
	az := seeded(t, adminWorkspaceRules())
	founder := claims(rbac.RoleFounder, "", rbac.PermissionsForRole(rbac.RoleFounder))

	// Founder's organization-level read is explicit (ScopeNone rule) and works
	// for any workspace, including B.
	require.NoError(t, az.AuthorizeClaims(founder, "GET", "/workspaces/"+workspaceB))

	// But the same founder has no rules for undeclared routes: no implicit
	// cross-workspace authority.
	require.ErrorIs(t, az.AuthorizeClaims(founder, "GET", "/api/workspaces/"+workspaceB+"/secrets"), ErrDenied)
}

func TestWorkspaceForgedPathDenied(t *testing.T) {
	az := seeded(t, adminWorkspaceRules())
	admin := claims(rbac.RoleWorkspaceAdmin, workspaceA, rbac.PermissionsForRole(rbac.RoleWorkspaceAdmin))

	// Forged workspace id in path (targets B while claims say A) -> denied.
	require.ErrorIs(t, az.AuthorizeClaims(admin, "GET", "/workspaces/"+workspaceB), ErrDenied)
	// A non-UUID garbage workspace id is also not equal to A -> denied.
	require.ErrorIs(t, az.AuthorizeClaims(admin, "GET", "/workspaces/not-a-uuid"), ErrDenied)
	// Missing path workspace -> denied.
	require.ErrorIs(t, az.AuthorizeClaims(admin, "GET", "/workspaces/"), ErrDenied)
}

func TestWorkspaceScopeSelfDeniedOnEmptyWorkspace(t *testing.T) {
	az := NewAuthorizer()
	az.AddRule("GET", "/workspaces", rbac.ScopeSelf, rbac.RoleWorkspaceMember)
	member := claims(rbac.RoleWorkspaceMember, "", rbac.PermissionsForRole(rbac.RoleWorkspaceMember))
	require.ErrorIs(t, az.AuthorizeClaims(member, "GET", "/workspaces"), ErrDenied,
		"a member/admin identity with no workspace must not be granted")
}

func TestMethodMismatchDenied(t *testing.T) {
	az := seeded(t, rbac.Rules())
	c := claims(rbac.RoleWorkspaceMember, workspaceFor(rbac.RoleWorkspaceMember), rbac.PermissionsForRole(rbac.RoleWorkspaceMember))
	require.ErrorIs(t, az.AuthorizeClaims(c, "POST", "/api/me"), ErrDenied)
}

func TestResourcePatternSingleIDMatching(t *testing.T) {
	_, ok := matchResource("/workspaces/{id}", "/workspaces/abc")
	require.True(t, ok)
	captured, ok := matchResource("/workspaces/{id}", "/workspaces/abc")
	require.True(t, ok)
	require.Equal(t, "abc", captured)

	_, ok = matchResource("/workspaces/{id}", "/workspaces")
	require.False(t, ok)

	_, ok = matchResource("/api/me", "/api/me")
	require.True(t, ok)

	_, ok = matchResource("/api/me/{id}", "/api/me")
	require.False(t, ok)

	_, ok = matchResource("/workspaces/{id}", "/workspaces/a/b")
	require.False(t, ok)
}

func adminWorkspaceRules() []rbac.Rule {
	return rbac.Rules()
}

const (
	workspaceA = "11111111-1111-1111-1111-111111111111"
	workspaceB = "22222222-2222-2222-2222-222222222222"
)

func workspaceFor(role rbac.Role) string {
	if role == rbac.RoleFounder {
		return ""
	}
	return workspaceA
}