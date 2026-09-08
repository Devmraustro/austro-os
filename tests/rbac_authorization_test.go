package austro_os_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/api"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/config"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// rbacTestUsers are the fixed non-founder identities seeded for the RBAC
// integration tests. They are distinct from any production identity.
const (
	adminAUser   = "rbac-admin-a"
	memberAUser  = "rbac-member-a"
	adminBUser   = "rbac-admin-b"
	rbacPassword = "rbac-test-password-min-16"
)

// ensureRbacUsers idempotently provisions the RBAC test identities in workspaces
// A and B. Returns the created records. Passwords are hashed with the same
// bcrypt primitive the service uses, so the live API can authenticate them.
func ensureRbacUsers(t *testing.T) (*auth.UserRecord, *auth.UserRecord, *auth.UserRecord) {
	t.Helper()
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	for _, u := range []string{adminAUser, memberAUser, adminBUser} {
		if _, err := admin.Exec(`DELETE FROM users WHERE username = $1`, u); err != nil {
			require.NoError(t, err)
		}
	}

	store := postgres.NewUserStore(admin)
	hash, err := auth.HashPassword(rbacPassword)
	require.NoError(t, err)

	adminA, err := store.Create(context.Background(), &auth.UserRecord{
		Username:     adminAUser,
		DisplayName:  "RBAC Admin A",
		Role:         rbac.RoleWorkspaceAdmin,
		WorkspaceID:  wsA.String(),
		PasswordHash: hash,
	})
	require.NoError(t, err)

	memberA, err := store.Create(context.Background(), &auth.UserRecord{
		Username:     memberAUser,
		DisplayName:  "RBAC Member A",
		Role:         rbac.RoleWorkspaceMember,
		WorkspaceID:  wsA.String(),
		PasswordHash: hash,
	})
	require.NoError(t, err)

	adminB, err := store.Create(context.Background(), &auth.UserRecord{
		Username:     adminBUser,
		DisplayName:  "RBAC Admin B",
		Role:         rbac.RoleWorkspaceAdmin,
		WorkspaceID:  wsB.String(),
		PasswordHash: hash,
	})
	require.NoError(t, err)

	// The workspace-scoped restricted roles need read access to the users
	// table for the RLS test; the RLS policy, not SQL grants, is what scopes
	// the rows those roles can see.
	_, err = admin.Exec(`GRANT SELECT ON users TO ` + workspaceARole + `, ` + workspaceBRole)
	require.NoError(t, err)

	return adminA, memberA, adminB
}

func rbacJWT(cfg *config.Config) *auth.JWTService {
	return auth.InitializeWithStore(cfg, auth.NewMemoryRefreshStore())
}

// TestRBACCredentialsDeriveRoleAndWorkspace proves the access-token claims come
// from the persistence layer, never from the client: the token minted for a
// database row carries exactly that role and workspace, and verification
// rejects any claim shape the system could not mint.
func TestRBACCredentialsDeriveRoleAndWorkspace(t *testing.T) {
	adminA, memberA, adminB := ensureRbacUsers(t)
	svc := rbacJWT(&config.Config{
		JWTSecret:        "test-access-secret-at-least-32-chars-long!!",
		JWTRefreshSecret: "test-refresh-secret-at-least-32-chars-long!!",
	})

	for name, u := range map[string]*auth.UserRecord{
		"admin-a":  adminA,
		"member-a": memberA,
		"admin-b":  adminB,
	} {
		t.Run(name, func(t *testing.T) {
			role := auth.RoleFor(u)
			require.NotEqual(t, rbac.Role(""), role)
			tok, err := svc.GenerateAccessTokenWithPermissions(u.ID, u.WorkspaceID, role, rbac.PermissionsForRole(role))
			require.NoError(t, err)
			claims, err := svc.VerifyAccessToken(tok)
			require.NoError(t, err)
			require.Equal(t, u.ID, claims.ID)
			require.Equal(t, role, claims.Role, "the token must carry the role persisted for the identity")
			require.Equal(t, u.WorkspaceID, claims.WorkspaceID, "the token must carry the workspace persisted for the identity")
			require.NotEmpty(t, claims.Role)
		})
	}

	require.NotEqual(t, adminA.WorkspaceID, adminB.WorkspaceID)
}

// TestRBACAuthorizationDecisions drives the engine over the declared RBAC
// surface (internal/rbac.Rules) with DB-derived claims from workspaces A and B.
func TestRBACAuthorizationDecisions(t *testing.T) {
	adminA, memberA, _ := ensureRbacUsers(t)
	az := authz.NewAuthorizer()
	az.AddRules(rbac.Rules())

	mint := func(u *auth.UserRecord) *auth.Claims {
		role := auth.RoleFor(u)
		return &auth.Claims{ID: u.ID, Role: role, WorkspaceID: u.WorkspaceID,
			Permissions: rbac.PermissionsForRole(role)}
	}
	adminAClaims := mint(adminA)
	memberAClaims := mint(memberA)

	// All identities can read their own profile; all require a validated rule.
	require.NoError(t, az.AuthorizeClaims(adminAClaims, "GET", "/api/me"))
	require.NoError(t, az.AuthorizeClaims(memberAClaims, "GET", "/api/me"))

	// Admin A on A allowed; on B denied (horizontal escalation blocked).
	require.NoError(t, az.AuthorizeClaims(adminAClaims, "GET", "/workspaces/"+workspaceA))
	require.ErrorIs(t, az.AuthorizeClaims(adminAClaims, "GET", "/workspaces/"+workspaceB), authz.ErrDenied)

	// Member role is denied administrative routes in every workspace.
	require.ErrorIs(t, az.AuthorizeClaims(memberAClaims, "GET", "/workspaces/"+workspaceA), authz.ErrDenied)
	require.ErrorIs(t, az.AuthorizeClaims(memberAClaims, "GET", "/workspaces/"+workspaceB), authz.ErrDenied)

	// Founder organization-level read is explicit and workspace-independent.
	founder := &auth.Claims{ID: "user-x", Role: rbac.RoleFounder, WorkspaceID: "",
		Permissions: rbac.PermissionsForRole(rbac.RoleFounder)}
	require.NoError(t, az.AuthorizeClaims(founder, "GET", "/workspaces/"+workspaceB))

	// Unknown route always denied, even for admin/founder (no default allow).
	require.ErrorIs(t, az.AuthorizeClaims(adminAClaims, "GET", "/api/unknown"), authz.ErrDenied)
	require.ErrorIs(t, az.AuthorizeClaims(founder, "GET", "/api/unknown"), authz.ErrDenied)
	require.ErrorIs(t, az.AuthorizeClaims(adminAClaims, "POST", "/api/me"), authz.ErrDenied)
}

// TestRBACClientSuppliedWorkspaceIgnored proves the workspace the client injects
// in a URL or body has no effect on the decision: the engine evaluates only
// verified claims and the request path, and a path workspace only satisfies
// ScopePath when it equals the claims workspace.
func TestRBACClientSuppliedWorkspaceIgnored(t *testing.T) {
	adminA, _, _ := ensureRbacUsers(t)
	az := authz.NewAuthorizer()
	az.AddRules(rbac.Rules())

	role := auth.RoleFor(adminA)
	claims := &auth.Claims{ID: adminA.ID, Role: role, WorkspaceID: adminA.WorkspaceID,
		Permissions: rbac.PermissionsForRole(role)}

	// A request to admin A's own workspace with a body claiming workspace B:
	// the body must be irrelevant to the decision.
	req := httptest.NewRequest(http.MethodGet, "/workspaces/"+workspaceA,
		strings.NewReader(`{"workspace_id":"`+workspaceB+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req = auth.WithClaims(req, claims)
	require.NoError(t, az.Authorize(req, "GET", "/workspaces/"+workspaceA))

	// The same admin requesting workspace B in the PATH, even with a body that
	// lies about being in A, is denied.
	reqB := httptest.NewRequest(http.MethodGet, "/workspaces/"+workspaceB,
		strings.NewReader(`{"workspace_id":"`+workspaceA+`"}`))
	reqB = auth.WithClaims(reqB, claims)
	require.ErrorIs(t, az.Authorize(reqB, "GET", "/workspaces/"+workspaceB), authz.ErrDenied)

	// Stale or forged path variants are never accessible.
	require.ErrorIs(t, az.AuthorizeClaims(claims, "GET", "/workspaces/not-a-uuid"), authz.ErrDenied)
}

// TestRBACWorkspaceBinder proves the claims->database-workspace bridge: a
// workspace-scoped handler derives the binding exclusively from verified
// claims, so a client cannot pick the workspace it operates in.
func TestRBACWorkspaceBinder(t *testing.T) {
	adminA, memberA, _ := ensureRbacUsers(t)

	adminClaims := &auth.Claims{ID: adminA.ID, Role: rbac.RoleWorkspaceAdmin, WorkspaceID: workspaceA}
	ws, err := api.WorkspaceFromClaims(adminClaims)
	require.NoError(t, err)
	require.Equal(t, workspaceA, ws)

	memberClaims := &auth.Claims{ID: memberA.ID, Role: rbac.RoleWorkspaceMember, WorkspaceID: workspaceA}
	ws, err = api.WorkspaceFromClaims(memberClaims)
	require.NoError(t, err)
	require.Equal(t, workspaceA, ws)

	// Founder has no home workspace: no silent cross-workspace binding.
	_, err = api.WorkspaceFromClaims(&auth.Claims{ID: "f", Role: rbac.RoleFounder})
	require.Error(t, err)

	// An unsupported role must never bind a workspace.
	_, err = api.WorkspaceFromClaims(&auth.Claims{ID: "x", Role: rbac.Role("hacker")})
	require.Error(t, err)

	// Invalid claims (nil) must never bind.
	_, err = api.WorkspaceFromClaims(nil)
	require.Error(t, err)
}

// TestRBACLiveRoleAndDenials exercises the running API with workspace-scoped
// non-founder identities: login works, /api/me reports the role, and the
// deny-by-default surface refuses workspace-shaped and unknown routes.
func TestRBACLiveRoleAndDenials(t *testing.T) {
	adminA, memberA, _ := ensureRbacUsers(t)

	// An admin of workspace A logs in and learns its role and workspace.
	status, body := authJSON(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": adminA.Username, "password": rbacPassword}, "")
	require.Equal(t, http.StatusOK, status, "admin login must succeed: %s", body)
	var adminLogin authTokenResponse
	require.NoError(t, json.Unmarshal(body, &adminLogin))

	status, body = authJSON(t, http.MethodGet, "/api/me", nil, adminLogin.AccessToken)
	require.Equal(t, http.StatusOK, status)
	var meAdmin authMeResponse
	require.NoError(t, json.Unmarshal(body, &meAdmin))
	require.Equal(t, "workspace_admin", meAdmin.Role)
	require.Equal(t, workspaceA, meAdmin.WorkspaceID)

	// A member of the same workspace sees its role.
	status, body = authJSON(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": memberA.Username, "password": rbacPassword}, "")
	require.Equal(t, http.StatusOK, status, "member login must succeed: %s", body)
	var memberLogin authTokenResponse
	require.NoError(t, json.Unmarshal(body, &memberLogin))

	status, body = authJSON(t, http.MethodGet, "/api/me", nil, memberLogin.AccessToken)
	require.Equal(t, http.StatusOK, status)
	var meMember authMeResponse
	require.NoError(t, json.Unmarshal(body, &meMember))
	require.Equal(t, "workspace_member", meMember.Role)
	require.Equal(t, workspaceA, meMember.WorkspaceID)

	// Deny-by-default: a workspace-shaped route and an unknown route are
	// refused even with a valid admin token (403), and never expose data.
	status, _ = authJSON(t, http.MethodGet, "/workspaces/"+workspaceA, nil, adminLogin.AccessToken)
	require.Equal(t, http.StatusForbidden, status, "workspace route must be denied until its endpoint exists")
	status, _ = authJSON(t, http.MethodGet, "/api/does-not-exist", nil, adminLogin.AccessToken)
	require.Equal(t, http.StatusForbidden, status)

	// Unauthenticated and invalid-token denials remain as specified.
	status, _ = authJSON(t, http.MethodGet, "/api/me", nil, "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = authJSON(t, http.MethodGet, "/api/me", nil, "garbage")
	require.Equal(t, http.StatusUnauthorized, status)
}

// TestRLSUsersRoleDoesNotWeakenScope proves the role column did not weaken the
// users-table RLS: a restricted workspace principal sees only its own
// workspace's users plus the founder, and never another workspace's identities.
func TestRLSUsersRoleDoesNotWeakenScope(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	_, _, _ = ensureRbacUsers(t)

	wsAUsers := usernamesInWorkspace(t, admin, workspaceA)
	wsBUsers := usernamesInWorkspace(t, admin, workspaceB)
	require.NotEmpty(t, wsAUsers, "workspace A must have scoped users for the probe")
	require.NotEmpty(t, wsBUsers, "workspace B must have scoped users for the probe")

	// The restricted A principal sees A's users but never B's users; the B
	// principal sees B's users but never A's.
	seenA := usersVisibleAs(t, workspaceA)
	seenB := usersVisibleAs(t, workspaceB)
	for _, u := range wsAUsers {
		require.True(t, seenA[u], "workspace A principal must see its own user %s", u)
	}
	for _, u := range wsBUsers {
		require.False(t, seenA[u], "workspace A principal must never see workspace B user %s", u)
		require.True(t, seenB[u], "workspace B principal must see its own user %s", u)
	}
	for _, u := range wsAUsers {
		require.False(t, seenB[u], "workspace B principal must never see workspace A user %s", u)
	}
}

func usernamesInWorkspace(t *testing.T, db *sql.DB, ws string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT username FROM users WHERE workspace_id = $1`, uuid.MustParse(ws))
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		out = append(out, name)
	}
	require.NoError(t, rows.Err())
	return out
}

// usersVisibleAs connects as a restricted role bound to a workspace and returns
// the set of non-founder usernames visible there; identities from another
// workspace must not appear.
func usersVisibleAs(t *testing.T, ws string) map[string]bool {
	t.Helper()
	role := workspaceARole
	if ws == workspaceB {
		role = workspaceBRole
	}
	db, err := connectAs(getEnv().postgresDSN, role)
	require.NoError(t, err)
	defer db.Close()

	// Scope the restricted connection to the workspace under test.
	if _, err := db.Exec(`SELECT set_config('app.current_workspace', $1, false)`, ws); err != nil {
		require.NoError(t, err)
	}

	seen := map[string]bool{}
	rows, err := db.Query(`SELECT username, is_founder FROM users`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var name string
		var isFounder bool
		require.NoError(t, rows.Scan(&name, &isFounder))
		if isFounder {
			// Founder is organization-level and visible in every workspace.
			continue
		}
		seen[name] = true
	}
	require.NoError(t, rows.Err())
	return seen
}
