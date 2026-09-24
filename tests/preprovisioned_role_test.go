package austro_os_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"austro-os/infrastructure/database"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// Pre-provisioned role topology.
//
// In this mode the platform (e.g. AlwaysData) has already created the
// runtime and admin roles on the PostgreSQL server. The application must use
// them without creating, altering or re-passwording them, and must fail fast
// at startup when a role is missing or carries an attribute the security
// contract forbids.
//
// The first tests in this file need no database (topology resolution is pure
// DSN/role reasoning). The tests marked with a live-database note run against
// the same PostgreSQL the rest of the suite uses: the owner connection is a
// superuser in every supported runtime (the compose service account and the
// CI service account), which is exactly what a platform operator has when
// pre-provisioning.

// Role names the live fixtures use. They are fixed so a fixture left behind
// by an interrupted run is recognisable, and they follow the same naming
// discipline the derived admin role does.
const (
	preRTRole    = "austro_pre_rt"
	preAdmRole   = "austro_pre_adm"
	preRTPass    = "pre-provisioned-runtime-cred"
	preAdmPass   = "pre-provisioned-admin-cred"
	goodRoleAtts = "LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE"

	// Dedicated canary workspaces, following the rtWorkspaceA/B discipline of
	// rls_runtime_role_test.go: this test must never touch the shared
	// workspaceA/workspaceB fixtures, whose row ids and names other tests in
	// the suite assert on.
	ppWorkspaceA = "33333333-3333-3333-3333-333333333333"
	ppWorkspaceB = "44444444-4444-4444-4444-444444444444"
)

// dsnWithRole rewrites a DSN's credential to the given role/password, keeping
// host, port, database and options.
func dsnWithRole(t *testing.T, base, role, password string) string {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	u.User = url.UserPassword(role, password)
	return u.String()
}

// ---------------------------------------------------------------------------
// Topology resolution (no database required).
// ---------------------------------------------------------------------------

// TestPreProvisionedTopologyHonorsDedicatedAdminDSN verifies the dedicated
// admin DSN is used exactly as supplied: its role name and its credential,
// not a derivation from the runtime DSN.
func TestPreProvisionedTopologyHonorsDedicatedAdminDSN(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES", "true")
	topo, err := database.ResolveTopologyWithAdmin(ppOwnerDSN, ppRuntimeDSN, ppAdminDSN)
	require.NoError(t, err)
	require.True(t, topo.PreProvisioned)
	require.Equal(t, "austro_app", topo.Runtime)
	require.Equal(t, "austro_app_admin", topo.Admin)
	require.Equal(t, ppAdminDSN, topo.AdminDSN,
		"the dedicated admin DSN must be used as supplied")
	require.Equal(t, ppRuntimeDSN, topo.RuntimeDSN)
	require.Equal(t, ppOwnerDSN, topo.OwnerDSN)
}

// TestPreProvisionedTopologyDerivesAdminWithoutDedicatedDSN verifies that with
// no dedicated admin DSN the classic derivation (role <runtime>_admin on the
// runtime DSN's host/credential) still applies.
func TestPreProvisionedTopologyDerivesAdminWithoutDedicatedDSN(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES", "true")
	topo, err := database.ResolveTopology(ppOwnerDSN, ppRuntimeDSN)
	require.NoError(t, err)
	require.True(t, topo.PreProvisioned)
	require.Equal(t, "austro_app_admin", topo.Admin)
	u, err := url.Parse(topo.AdminDSN)
	require.NoError(t, err)
	require.Equal(t, "austro_app_admin", u.User.Username())
	pw, _ := u.User.Password()
	require.Equal(t, "pp-runtime-cred-5678", pw,
		"a derived admin DSN reuses the runtime DSN's credential, as before")
	require.Equal(t, u.Host, u2Host(t, ppRuntimeDSN))
}

func u2Host(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	return u.Host
}

// TestPreProvisionedFlagDefaultsOff verifies the pre-provisioned mode is opt-
// in: with the flag unset the topology resolves as self-provisioned.
func TestPreProvisionedFlagDefaultsOff(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES", "")
	topo, err := database.ResolveTopology(ppOwnerDSN, ppRuntimeDSN)
	require.NoError(t, err)
	require.False(t, topo.PreProvisioned)
}

// TestPreProvisionedTopologyInvalidAdminRoleName verifies a dedicated admin
// DSN whose role is not a supported identifier is rejected before anything
// touches the database.
func TestPreProvisionedTopologyInvalidAdminRoleName(t *testing.T) {
	bad := "postgres://9lives:pp-admin-cred-9012@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require"
	_, err := database.ResolveTopologyWithAdmin(ppOwnerDSN, ppRuntimeDSN, bad)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a supported identifier")
}

// TestPreProvisionedTopologyAdminEqualsOwner verifies an admin that is the
// schema owner (same role, different credential — the DSN strings differ, so
// only a role-level check catches it) is rejected.
func TestPreProvisionedTopologyAdminEqualsOwner(t *testing.T) {
	admin := "postgres://austro:other-cred-4242@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require"
	_, err := database.ResolveTopologyWithAdmin(ppOwnerDSN, ppRuntimeDSN, admin)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must differ from the schema owner")
}

// TestPreProvisionedTopologyAdminEqualsRuntime verifies an admin that is the
// application runtime role is rejected.
func TestPreProvisionedTopologyAdminEqualsRuntime(t *testing.T) {
	admin := "postgres://austro_app:other-cred-4242@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require"
	_, err := database.ResolveTopologyWithAdmin(ppOwnerDSN, ppRuntimeDSN, admin)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must differ from the application runtime role")
}

// ---------------------------------------------------------------------------
// Live database coverage.
//
// These tests need the suite's PostgreSQL (owner connection must be able to
// create roles, i.e. be a superuser: the compose and CI service accounts
// both are). They fail with a connection error when no database is reachable,
// exactly like the rest of this suite.
// ---------------------------------------------------------------------------

// preProvisionedFixture pre-creates the two serving roles the way a platform
// would (owner connection only), with the exact AlwaysData attributes: LOGIN,
// NOSUPERUSER, NOBYPASSRLS, NOCREATEDB, NOCREATEROLE. An empty attr string
// skips creating that role, which is how the "missing role" scenarios are
// built. The DSNs returned are the platform's credentials; nothing in the
// application is allowed to change them.
func preProvisionedFixture(t *testing.T, rtAttrs, admAttrs string) (string, string) {
	t.Helper()
	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	t.Cleanup(func() {
		// The roles hold grants from the bootstrap, so free the dependencies
		// before dropping; both statements are no-ops for a role that was
		// never created.
		for _, role := range []string{preRTRole, preAdmRole} {
			_, _ = owner.Exec("DROP OWNED BY " + role)
			_, _ = owner.Exec("DROP ROLE IF EXISTS " + role)
		}
		_ = owner.Close()
	})
	// Remove fixtures from an interrupted earlier run.
	_, _ = owner.Exec("DROP OWNED BY " + preRTRole)
	_, _ = owner.Exec("DROP ROLE IF EXISTS " + preRTRole)
	_, _ = owner.Exec("DROP OWNED BY " + preAdmRole)
	_, _ = owner.Exec("DROP ROLE IF EXISTS " + preAdmRole)

	if rtAttrs != "" {
		// #nosec G201 -- role name is a fixed constant; attrs come from this test file.
		_, err = owner.Exec("CREATE ROLE " + preRTRole + " " + rtAttrs + " PASSWORD '" + preRTPass + "'")
		require.NoError(t, err)
	}
	if admAttrs != "" {
		// #nosec G201 -- role name is a fixed constant; attrs come from this test file.
		_, err = owner.Exec("CREATE ROLE " + preAdmRole + " " + admAttrs + " PASSWORD '" + preAdmPass + "'")
		require.NoError(t, err)
	}
	base := getEnv().postgresDSN
	return dsnWithRole(t, base, preRTRole, preRTPass), dsnWithRole(t, base, preAdmRole, preAdmPass)
}

// TestPreProvisionedModeEndToEnd is the real integration path: it pre-creates
// the two serving roles exactly the way AlwaysData provisioned them, then runs
// the production bootstrap (the same function the process runs at startup)
// against that topology, and proves the application serves on the
// pre-provisioned credentials while the security architecture stays intact.
func TestPreProvisionedModeEndToEnd(t *testing.T) {
	rtDSN, admDSN := preProvisionedFixture(t, goodRoleAtts, goodRoleAtts)
	t.Setenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES", "true")

	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()

	topo, err := database.ResolveTopologyWithAdmin(getEnv().postgresDSN, rtDSN, admDSN)
	require.NoError(t, err)
	require.True(t, topo.PreProvisioned)

	// The same function the process runs at startup.
	require.NoError(t, database.Bootstrap(owner, topo))

	rt := runtimeDB(t, topo)
	adm := adminDB(t, topo)

	// Bootstrap re-creates the organization-level policies (org_admin_policy
	// on workspaces, audit_org_policy on audit_events) TO the topology's
	// admin role. This test bootstrapped with a fixture topology, so those
	// policies now name the fixture admin role; once the fixture roles are
	// dropped, no live role matches them and every administrative-pool query
	// in the rest of the suite (and the running API) would see nothing.
	// Restore them to the suite's real topology admin role before finishing.
	t.Cleanup(func() {
		real, err := database.ResolveTopology(getEnv().postgresDSN, os.Getenv("AUSTRO_POSTGRES_RUNTIME_DSN"))
		if err != nil {
			t.Errorf("resolve real topology for policy restore: %v", err)
			return
		}
		restore, err := sql.Open("pgx", getEnv().postgresDSN)
		if err != nil {
			t.Errorf("open owner for policy restore: %v", err)
			return
		}
		defer restore.Close()
		stmts := []string{
			"DROP POLICY IF EXISTS org_admin_policy ON workspaces",
			fmt.Sprintf("CREATE POLICY org_admin_policy ON workspaces TO %s USING (true)", real.Admin),
			"DROP POLICY IF EXISTS audit_org_policy ON audit_events",
			fmt.Sprintf("CREATE POLICY audit_org_policy ON audit_events TO %s USING (true)", real.Admin),
		}
		for _, s := range stmts {
			if _, err := restore.Exec(s); err != nil {
				t.Errorf("restore organization-level policy: %v", err)
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, database.VerifyRuntimeSecurity(ctx, rt, topo.Runtime),
		"the pre-provisioned runtime role must prove the full isolation boundary")
	require.NoError(t, database.VerifyAdminRole(ctx, adm, topo.Admin),
		"the dedicated admin DSN must authenticate as the expected admin role")

	// The bootstrap must not have mutated the platform's roles: their
	// attributes are still exactly what was pre-provisioned.
	for _, role := range []string{preRTRole, preAdmRole} {
		var super, bypass, createrole, createdb, login bool
		require.NoError(t, owner.QueryRow(`
			SELECT rolsuper, rolbypassrls, rolcreaterole, rolcreatedb, rolcanlogin
			FROM pg_roles WHERE rolname = $1`, role).
			Scan(&super, &bypass, &createrole, &createdb, &login))
		require.False(t, super, "%s must remain a non-superuser", role)
		require.False(t, bypass, "%s must remain without BYPASSRLS", role)
		require.False(t, createrole, "%s must remain without CREATEROLE", role)
		require.False(t, createdb, "%s must remain without CREATEDB", role)
		require.True(t, login, "%s must remain a LOGIN role", role)
	}

	// Passwords were never touched: the platform's original credentials must
	// still authenticate after the full bootstrap.
	for _, dsn := range []string{rtDSN, admDSN} {
		again, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		require.NoError(t, again.Ping(), "the pre-provisioned credential must still work")
		_ = again.Close()
	}

	// The pre-provisioned runtime role is still workspace-isolated from the
	// very connection it serves on. Both canary workspaces exist; from B's
	// session B must be visible and A must not be.
	seedWorkspace(t, topo, ppWorkspaceA, "pre-provisioned-other")
	seedWorkspace(t, topo, ppWorkspaceB, "pre-provisioned-canary")
	tx := bindWorkspace(t, rt, ppWorkspaceB)
	var own, other int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM workspaces WHERE id = $1`, ppWorkspaceB).Scan(&own))
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM workspaces WHERE id = $1`, ppWorkspaceA).Scan(&other))
	require.GreaterOrEqual(t, own, 1, "the runtime role must see its own workspace")
	require.Equal(t, 0, other,
		"the pre-provisioned runtime role must not see another workspace's rows")
}

// TestPreProvisionedBootstrapFailsFast verifies each attribute the security
// contract forbids is refused at bootstrap, before the process serves
// anything. The role is pre-created with exactly one broken attribute, so
// the error names the deviation rather than a generic failure.
func TestPreProvisionedBootstrapFailsFast(t *testing.T) {
	cases := []struct {
		name     string
		rtAttrs  string
		admAttrs string
		wantMsg  string
	}{
		{"missing-runtime-role", "", goodRoleAtts, `"` + preRTRole + `" does not exist`},
		{"missing-admin-role", goodRoleAtts, "", `"` + preAdmRole + `" does not exist`},
		{"superuser-runtime", "LOGIN SUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE", goodRoleAtts, "superuser"},
		{"bypass-rls-admin", goodRoleAtts, "LOGIN NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE", "BYPASSRLS"},
		{"no-login-runtime", "NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE", goodRoleAtts, "LOGIN role"},
		{"createrole-runtime", "LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB CREATEROLE", goodRoleAtts, "CREATEROLE"},
		{"createdb-admin", goodRoleAtts, "LOGIN NOSUPERUSER NOBYPASSRLS CREATEDB NOCREATEROLE", "CREATEDB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rtDSN, admDSN := preProvisionedFixture(t, tc.rtAttrs, tc.admAttrs)
			t.Setenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES", "true")

			owner, err := sql.Open("pgx", getEnv().postgresDSN)
			require.NoError(t, err)
			defer owner.Close()

			topo, err := database.ResolveTopologyWithAdmin(getEnv().postgresDSN, rtDSN, admDSN)
			require.NoError(t, err)

			err = database.Bootstrap(owner, topo)
			require.Error(t, err, "bootstrap must fail fast on %s", tc.name)
			require.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}
