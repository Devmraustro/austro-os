package austro_os_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/rbac"

	"github.com/stretchr/testify/require"
)

// claims returns an authenticated principal with a single explicit permission:
// action "read" on resource "acme:report", role workspace_member, workspace A.
func rbacClaims() *auth.Claims {
	return &auth.Claims{
		ID:          "user-1",
		Role:        rbac.RoleWorkspaceMember,
		WorkspaceID: workspaceA,
		Permissions: map[string][]string{
			"read": {"acme:report"},
		},
	}
}

// rbacAuthorizer seeds an authorizer with the one explicit rule used by these
// tests: workspace members may read acme:report within their own workspace.
func rbacAuthorizer() *authz.Authorizer {
	a := authz.NewAuthorizer()
	a.AddRule("read", "acme:report", rbac.ScopeSelf, rbac.RoleWorkspaceMember)
	return a
}

func TestAuthzDenyByDefault(t *testing.T) {
	a := authz.NewAuthorizer()

	// An empty authorizer has no explicit rules: nothing may be allowed.
	t.Run("no explicit rule is denied", func(t *testing.T) {
		err := a.AuthorizeClaims(rbacClaims(), "read", "acme:report")
		require.ErrorIs(t, err, authz.ErrDenied)
	})
}

func TestAuthzUnknownActionDenied(t *testing.T) {
	a := rbacAuthorizer()
	// An explicit rule exists only for action "read".
	err := a.AuthorizeClaims(rbacClaims(), "delete", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzUnknownResourceDenied(t *testing.T) {
	a := rbacAuthorizer()

	// The caller has the action but requests a resource with no explicit rule.
	err := a.AuthorizeClaims(rbacClaims(), "read", "acme:secrets")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzMissingPermissionDenied(t *testing.T) {
	a := authz.NewAuthorizer()
	a.AddRule("write", "acme:report", rbac.ScopeSelf, rbac.RoleWorkspaceMember)

	// The rule exists, but the principal holds no "write" permission.
	err := a.AuthorizeClaims(rbacClaims(), "write", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzWrongWorkspaceDenied(t *testing.T) {
	// ScopePath: the workspace in the PATH must equal the caller's token
	// workspace. A member of A trying to reach a resource identified by
	// workspace B (or a garbage id) is denied; headers/body can never help.
	p := authz.NewAuthorizer()
	p.AddRule("GET", "/reports/{id}", rbac.ScopePath, rbac.RoleWorkspaceMember)
	claims := rbacClaims()
	claims.Permissions["GET"] = []string{"/reports/{id}"}
	require.NoError(t, p.AuthorizeClaims(claims, "GET", "/reports/"+workspaceA),
		"the caller's own workspace is allowed")
	require.ErrorIs(t, p.AuthorizeClaims(claims, "GET", "/reports/"+workspaceB), authz.ErrDenied)
	require.ErrorIs(t, p.AuthorizeClaims(claims, "GET", "/reports/not-a-uuid"), authz.ErrDenied)

	// ScopeSelf: the operation happens in the caller's own claims-bound
	// workspace, and no target workspace appears in the request for a client
	// to switch. Each caller is therefore bound to its own token workspace.
	s := authz.NewAuthorizer()
	s.AddRule("read", "acme:report", rbac.ScopeSelf, rbac.RoleWorkspaceMember)
	require.NoError(t, s.AuthorizeClaims(rbacClaims(), "read", "acme:report"))
	claimsB := rbacClaims()
	claimsB.WorkspaceID = workspaceB
	require.NoError(t, s.AuthorizeClaims(claimsB, "read", "acme:report"),
		"a member only ever operates in the workspace its token binds it to")
	claimsNone := rbacClaims()
	claimsNone.WorkspaceID = ""
	require.ErrorIs(t, s.AuthorizeClaims(claimsNone, "read", "acme:report"), authz.ErrDenied,
		"a member claim without a home workspace cannot be minted and is denied")
}

func TestAuthzExplicitRuleAllows(t *testing.T) {
	a := rbacAuthorizer()

	err := a.AuthorizeClaims(rbacClaims(), "read", "acme:report")
	require.NoError(t, err)
}

func TestAuthzRoleDenied(t *testing.T) {
	a := rbacAuthorizer()
	// A founder is not in the rule's allowed roles.
	claims := rbacClaims()
	claims.Role = rbac.RoleFounder
	err := a.AuthorizeClaims(claims, "read", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzUnsupportedRoleDenied(t *testing.T) {
	a := rbacAuthorizer()
	claims := rbacClaims()
	claims.Role = rbac.Role("superadmin")
	err := a.AuthorizeClaims(claims, "read", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzOrgScopeAllowsWithoutWorkspace(t *testing.T) {
	a := authz.NewAuthorizer()
	// A founder rule with no workspace binding covers any workspace.
	a.AddRule("read", "acme:report", rbac.ScopeNone, rbac.RoleFounder)

	claims := rbacClaims()
	claims.Role = rbac.RoleFounder
	claims.WorkspaceID = ""
	err := a.AuthorizeClaims(claims, "read", "acme:report")
	require.NoError(t, err)
}

func TestAuthzUnauthenticatedDenied(t *testing.T) {
	a := rbacAuthorizer()

	// A request without claims (no authenticated principal) is denied.
	req := httptest.NewRequest(http.MethodGet, "/acme/report", nil)
	err := a.Authorize(req, "read", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)

	// A second, independent unauthenticated request must also be denied --
	// never allowed by any implicit fallback.
	reqNoAuth := httptest.NewRequest(http.MethodGet, "/acme/report", nil)
	err = a.Authorize(reqNoAuth, "read", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzHTTPPathWithAuthenticatedClaims(t *testing.T) {
	a := authz.NewAuthorizer()
	a.AddRule("GET", "/acme/report", rbac.ScopeNone, rbac.RoleWorkspaceMember)

	granted := rbacClaims()
	granted.Permissions = map[string][]string{"GET": {"/acme/report"}}

	// Authenticated request with the correct permission + role is allowed.
	req := httptest.NewRequest(http.MethodGet, "/acme/report", nil)
	req = auth.WithClaims(req, granted)
	err := a.Authorize(req, "GET", "/acme/report")
	require.NoError(t, err)

	// An authenticated principal that is NOT granted the permission for the
	// requested resource is still denied.
	reqUngranted := httptest.NewRequest(http.MethodGet, "/acme/report", nil)
	reqUngranted = auth.WithClaims(reqUngranted, rbacClaims())
	err = a.Authorize(reqUngranted, "GET", "/acme/report")
	require.ErrorIs(t, err, authz.ErrDenied)

	// A resource with no explicit rule is denied, even though the principal is
	// authenticated.
	reqOther := httptest.NewRequest(http.MethodGet, "/acme/other", nil)
	reqOther = auth.WithClaims(reqOther, granted)
	err = a.Authorize(reqOther, "GET", "/acme/other")
	require.ErrorIs(t, err, authz.ErrDenied)
}

// TestAuthzNoPermissiveFallback asserts there is no default-allow path: the
// only way to be permitted is a matching explicit rule plus the matching
// permission plus a matching role plus a matching workspace scope.
func TestAuthzNoPermissiveFallback(t *testing.T) {
	a := authz.NewAuthorizer()
	a.AddRule("read", "acme:report", rbac.ScopeSelf, rbac.RoleWorkspaceMember)
	a.AddRule("read", "acme:public", rbac.ScopeNone, rbac.RoleWorkspaceMember)

	// Granted permissions only cover acme:report; acme:public is a rule the
	// principal does not carry, so it must be denied.
	err := a.AuthorizeClaims(rbacClaims(), "read", "acme:public")
	require.ErrorIs(t, err, authz.ErrDenied)

	// A denial must always be the sentinel error, never nil, for any unlisted
	// action/resource pairing even when the action and resource individually
	// appear elsewhere.
	err = a.AuthorizeClaims(rbacClaims(), "read", "acme:entirely-new")
	require.ErrorIs(t, err, authz.ErrDenied)
	err = a.AuthorizeClaims(rbacClaims(), "archive", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}
