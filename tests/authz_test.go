package austro_os_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"austro-os/internal/auth"
	"austro-os/internal/authz"

	"github.com/stretchr/testify/require"
)

// claims returns an authenticated principal with a single explicit permission:
// action "read" on resource "acme:report". Workspace is set to workspaceA.
func authzClaims() *auth.Claims {
	return &auth.Claims{
		ID:          "user-1",
		WorkspaceID: workspaceA,
		Permissions: map[string][]string{
			"read": {"acme:report"},
		},
	}
}

func TestAuthzDenyByDefault(t *testing.T) {
	a := authz.NewAuthorizer()

	// An empty authorizer has no explicit rules: nothing may be allowed.
	t.Run("no explicit rule is denied", func(t *testing.T) {
		err := a.AuthorizeClaims(authzClaims(), "read", "acme:report")
		require.ErrorIs(t, err, authz.ErrDenied)
	})
}

func TestAuthzUnknownActionDenied(t *testing.T) {
	a := authz.NewAuthorizer()
	// An explicit rule exists only for action "read".
	a.AddRule("read", "acme:report", workspaceA)

	err := a.AuthorizeClaims(authzClaims(), "delete", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzUnknownResourceDenied(t *testing.T) {
	a := authz.NewAuthorizer()
	a.AddRule("read", "acme:report", workspaceA)

	// The caller has the action but requests a resource with no explicit rule.
	err := a.AuthorizeClaims(authzClaims(), "read", "acme:secrets")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzMissingPermissionDenied(t *testing.T) {
	a := authz.NewAuthorizer()
	a.AddRule("write", "acme:report", workspaceA)

	// The rule exists, but the principal holds no "write" permission.
	err := a.AuthorizeClaims(authzClaims(), "write", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}

func TestAuthzWrongWorkspaceDenied(t *testing.T) {
	a := authz.NewAuthorizer()
	// Rule is scoped to workspaceA only.
	a.AddRule("read", "acme:report", workspaceA)

	// Principal is in workspaceB, so the workspace scope does not match.
	claims := authzClaims()
	claims.WorkspaceID = workspaceB
	err := a.AuthorizeClaims(claims, "read", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)

	// Sanity: the same principal in the correct workspace is allowed.
	claims.WorkspaceID = workspaceA
	err = a.AuthorizeClaims(claims, "read", "acme:report")
	require.NoError(t, err)
}

func TestAuthzExplicitRuleAllows(t *testing.T) {
	a := authz.NewAuthorizer()
	a.AddRule("read", "acme:report", workspaceA)

	err := a.AuthorizeClaims(authzClaims(), "read", "acme:report")
	require.NoError(t, err)
}

func TestAuthzAllowAnyWorkspaceScope(t *testing.T) {
	a := authz.NewAuthorizer()
	// A rule declared without a workspace list covers any workspace.
	a.AddRule("read", "acme:report")

	claims := authzClaims()
	claims.WorkspaceID = workspaceB
	err := a.AuthorizeClaims(claims, "read", "acme:report")
	require.NoError(t, err)
}

func TestAuthzUnauthenticatedDenied(t *testing.T) {
	a := authz.NewAuthorizer()
	a.AddRule("read", "acme:report", workspaceA)

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
	a.AddRule("read", "/acme/report", workspaceA)

	// A principal granted the permission for the HTTP-path resource.
	granted := authzClaims()
	granted.Permissions = map[string][]string{"read": {"/acme/report"}}

	// Authenticated request with the correct permission + workspace is allowed.
	req := httptest.NewRequest(http.MethodGet, "/acme/report", nil)
	req = auth.WithClaims(req, granted)
	err := a.Authorize(req, "read", "/acme/report")
	require.NoError(t, err)

	// An authenticated principal that is NOT granted the permission for the
	// requested resource is still denied -- the permission grant must match the
	// exact HTTP-path resource.
	reqUngranted := httptest.NewRequest(http.MethodGet, "/acme/report", nil)
	reqUngranted = auth.WithClaims(reqUngranted, authzClaims())
	err = a.Authorize(reqUngranted, "read", "/acme/report")
	require.ErrorIs(t, err, authz.ErrDenied)

	// The same request against a resource that has no explicit rule is denied,
	// even though the principal is authenticated and has a valid bearer-esque
	// claim set.
	reqOther := httptest.NewRequest(http.MethodGet, "/acme/other", nil)
	reqOther = auth.WithClaims(reqOther, granted)
	err = a.Authorize(reqOther, "read", "/acme/other")
	require.ErrorIs(t, err, authz.ErrDenied)
}

// TestAuthzNoPermissiveFallback asserts there is no default-allow path: an
// authorizer with rules seeded still denies any combination that is not
// explicitly listed. The only way to be permitted is a matching explicit rule
// plus the matching permission plus a matching workspace scope.
func TestAuthzNoPermissiveFallback(t *testing.T) {
	a := authz.NewAuthorizer()
	a.AddRule("read", "acme:report", workspaceA)
	a.AddRule("read", "acme:public")

	// Granted permissions only cover acme:report; acme:public is a rule the
	// principal does not carry, so it must be denied.
	err := a.AuthorizeClaims(authzClaims(), "read", "acme:public")
	require.ErrorIs(t, err, authz.ErrDenied)

	// A denial must always be the sentinel error, never nil, for any unlisted
	// action/resource pairing even when the action and resource individually
	// appear elsewhere.
	err = a.AuthorizeClaims(authzClaims(), "read", "acme:entirely-new")
	require.ErrorIs(t, err, authz.ErrDenied)
	err = a.AuthorizeClaims(authzClaims(), "archive", "acme:report")
	require.ErrorIs(t, err, authz.ErrDenied)
}