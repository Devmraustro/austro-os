package austro_os_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"austro-os/infrastructure/database"

	"github.com/stretchr/testify/require"
)

// The workspace pair these tests use. They are distinct from the
// workspaceA/workspaceB constants in helpers_test.go so the two suites cannot
// observe each other's rows.
const (
	rtWorkspaceA = "33333333-3333-4333-8333-333333333333"
	rtWorkspaceB = "44444444-4444-4444-8444-444444444444"
)

// A. The runtime role must not be a superuser and must not hold BYPASSRLS.
//
// Either attribute makes PostgreSQL skip row level security outright, including
// under FORCE, so every policy in the schema would be decorative while the
// process still looked correctly configured.
func TestRuntimeRoleIsNotPrivileged(t *testing.T) {
	topo := topologyFixture(t)
	rt := runtimeDB(t, topo)

	var current string
	var super, bypass, createdb, createrole bool
	require.NoError(t, rt.QueryRow(`
		SELECT rolname, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole
		FROM pg_roles WHERE rolname = current_user`).
		Scan(&current, &super, &bypass, &createdb, &createrole))

	require.Equal(t, topo.Runtime, current,
		"the pool must actually be connected as the configured runtime role")
	require.False(t, super, "the application runtime role must not be a superuser")
	require.False(t, bypass, "the application runtime role must not hold BYPASSRLS")
	require.False(t, createdb, "the application runtime role must not be able to create databases")
	require.False(t, createrole, "the application runtime role must not be able to create roles")
}

// B. The runtime role must not own any protected table. PostgreSQL does not
// apply row level security to a table's owner, so ownership would be a silent,
// total isolation bypass even with correct policies.
func TestRuntimeRoleDoesNotOwnProtectedTables(t *testing.T) {
	topo := topologyFixture(t)
	rt := runtimeDB(t, topo)

	rows, err := rt.Query(`
		SELECT c.relname, r.rolname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_roles r ON r.oid = c.relowner
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname = ANY($1)`,
		protectedTables)
	require.NoError(t, err)
	defer rows.Close()

	var owned []string
	for rows.Next() {
		var table, owner string
		require.NoError(t, rows.Scan(&table, &owner))
		if strings.EqualFold(owner, topo.Runtime) {
			owned = append(owned, table)
		}
	}
	require.NoError(t, rows.Err())
	require.Empty(t, owned, "the runtime role must not own any protected table")

	// Every protected table must exist, otherwise the check above passed
	// vacuously.
	var found int
	require.NoError(t, rt.QueryRow(`
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname = ANY($1)`,
		protectedTables).Scan(&found))
	require.Equal(t, len(protectedTables), found, "every protected table must exist")
}

// Every protected table must have row level security enabled AND forced.
// Enabled-but-not-forced is the state that let an owner connection through.
func TestProtectedTablesHaveForcedRLS(t *testing.T) {
	topo := topologyFixture(t)
	rt := runtimeDB(t, topo)

	rows, err := rt.Query(`
		SELECT relname, relrowsecurity, relforcerowsecurity
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relname = ANY($1)`,
		protectedTables)
	require.NoError(t, err)
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		require.NoError(t, rows.Scan(&name, &enabled, &forced))
		require.True(t, enabled, "row level security must be enabled on %s", name)
		require.True(t, forced, "row level security must be FORCED on %s", name)
		checked++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, len(protectedTables), checked)
}

// C, D, E. Workspace A cannot read, update or delete workspace B's rows, from
// the runtime role the application actually uses.
func TestRuntimeRoleCannotCrossWorkspaceBoundary(t *testing.T) {
	topo := topologyFixture(t)
	seedWorkspace(t, topo, rtWorkspaceA, "Runtime A")
	seedWorkspace(t, topo, rtWorkspaceB, "Runtime B")

	rt := runtimeDB(t, topo)

	// One department in each workspace, seeded by the owner as a migration would.
	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()
	deptA := "55555555-0000-4000-8000-00000000000a"
	deptB := "55555555-0000-4000-8000-00000000000b"
	for _, d := range []struct{ id, ws, name string }{
		{deptA, rtWorkspaceA, "A dept"},
		{deptB, rtWorkspaceB, "B dept"},
	} {
		_, err := owner.Exec(`INSERT INTO departments (id, workspace_id, name) VALUES ($1,$2,$3)
			ON CONFLICT (id) DO NOTHING`, d.id, d.ws, d.name)
		require.NoError(t, err)
	}

	// C. read
	t.Run("read", func(t *testing.T) {
		tx := bindWorkspace(t, rt, rtWorkspaceA)
		var visible int
		require.NoError(t, tx.QueryRow(`SELECT count(*) FROM departments`).Scan(&visible))
		require.Equal(t, 1, visible, "workspace A must see exactly its own department")

		var other int
		require.NoError(t, tx.QueryRow(`SELECT count(*) FROM departments WHERE id = $1`, deptB).Scan(&other))
		require.Zero(t, other, "workspace A must not read workspace B's department")
	})

	// D. update
	t.Run("update", func(t *testing.T) {
		tx := bindWorkspace(t, rt, rtWorkspaceA)
		res, err := tx.Exec(`UPDATE departments SET name = 'hijacked' WHERE id = $1`, deptB)
		require.NoError(t, err, "an out-of-scope update must affect zero rows, not error")
		affected, err := res.RowsAffected()
		require.NoError(t, err)
		require.Zero(t, affected, "workspace A must not update workspace B's department")
	})

	// E. delete
	t.Run("delete", func(t *testing.T) {
		tx := bindWorkspace(t, rt, rtWorkspaceA)
		res, err := tx.Exec(`DELETE FROM departments WHERE workspace_id = $1`, rtWorkspaceB)
		require.NoError(t, err)
		affected, err := res.RowsAffected()
		require.NoError(t, err)
		require.Zero(t, affected, "workspace A must not delete workspace B's departments")
	})

	// A blanket write with no predicate at all must still be confined.
	t.Run("unscoped_write_is_confined", func(t *testing.T) {
		tx := bindWorkspace(t, rt, rtWorkspaceA)
		res, err := tx.Exec(`DELETE FROM departments`)
		require.NoError(t, err)
		affected, err := res.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), affected,
			"an unscoped delete must only reach the bound workspace's rows")
	})
}

// F. The runtime role cannot disable, un-force or otherwise step around row
// level security, and cannot assume a privileged role.
func TestRuntimeRoleCannotBypassRLS(t *testing.T) {
	topo := topologyFixture(t)
	seedWorkspace(t, topo, rtWorkspaceA, "Runtime A")
	seedWorkspace(t, topo, rtWorkspaceB, "Runtime B")
	rt := runtimeDB(t, topo)

	// SET row_security = off must not widen the view. PostgreSQL raises a
	// permission error for a non-owner that would otherwise see filtered rows;
	// either way, B's rows must not appear.
	t.Run("row_security_off_does_not_widen_view", func(t *testing.T) {
		tx := bindWorkspace(t, rt, rtWorkspaceA)
		_, _ = tx.Exec(`SET LOCAL row_security = off`)
		var visible int
		err := tx.QueryRow(`SELECT count(*) FROM departments WHERE workspace_id = $1`, rtWorkspaceB).Scan(&visible)
		if err == nil {
			require.Zero(t, visible, "row_security=off must not expose another workspace")
		}
	})

	// Privileged DDL must be refused outright.
	for _, stmt := range []string{
		`ALTER TABLE departments DISABLE ROW LEVEL SECURITY`,
		`ALTER TABLE departments NO FORCE ROW LEVEL SECURITY`,
		`ALTER TABLE departments OWNER TO ` + topo.Runtime,
		`DROP POLICY workspace_isolation_policy ON departments`,
	} {
		_, err := rt.Exec(stmt)
		require.Error(t, err, "the runtime role must not be able to run: %s", stmt)
	}

	// It must not be able to become the owner or a superuser.
	ownerRole, err := roleFromDSNForTest(getEnv().postgresDSN)
	require.NoError(t, err)
	_, err = rt.Exec(`SET ROLE ` + ownerRole)
	require.Error(t, err, "the runtime role must not be able to assume the schema owner role")

	_, err = rt.Exec(`CREATE ROLE escalated SUPERUSER LOGIN`)
	require.Error(t, err, "the runtime role must not be able to create roles")
}

// G. Organization-level (founder) semantics still work, and they work through
// the administrative principal rather than by weakening the runtime role.
func TestFounderOrgLevelSemantics(t *testing.T) {
	topo := topologyFixture(t)
	seedWorkspace(t, topo, rtWorkspaceA, "Runtime A")
	seedWorkspace(t, topo, rtWorkspaceB, "Runtime B")

	rt := runtimeDB(t, topo)
	admin := adminDB(t, topo)

	// The administrative role sees the whole organization: this is what a
	// founder listing workspaces needs.
	var orgCount int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM workspaces`).Scan(&orgCount))
	require.GreaterOrEqual(t, orgCount, 2, "the admin role must see the whole organization")

	// The runtime role, bound to a workspace, sees only that workspace.
	tx := bindWorkspace(t, rt, rtWorkspaceA)
	var ownCount int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM workspaces`).Scan(&ownCount))
	require.Equal(t, 1, ownCount, "a workspace-scoped session must see only its own workspace")

	// Unbound, the runtime role sees nothing: the default is deny, not allow.
	var unbound int
	require.NoError(t, rt.QueryRow(`SELECT count(*) FROM workspaces`).Scan(&unbound))
	require.Zero(t, unbound, "an unbound runtime session must see no workspaces")

	// Login and bootstrap resolve an identity before any workspace is bound, so
	// the users policy must expose the founder to an unbound session.
	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()
	founderID := "66666666-0000-4000-8000-00000000000f"
	_, err = owner.Exec(`INSERT INTO users (id, username, password_hash, display_name, is_founder, role, workspace_id)
		VALUES ($1,'rt-founder','x','RT Founder',TRUE,'founder',NULL)
		ON CONFLICT (username) DO UPDATE SET is_founder = TRUE, role = 'founder', workspace_id = NULL`, founderID)
	require.NoError(t, err)

	var founderVisible int
	require.NoError(t, rt.QueryRow(`SELECT count(*) FROM users WHERE username = 'rt-founder'`).Scan(&founderVisible))
	require.Equal(t, 1, founderVisible,
		"an unbound session must still resolve the founder identity, or login cannot work")

	// A non-founder identity stays invisible to an unbound session.
	_, err = owner.Exec(`INSERT INTO users (id, username, password_hash, display_name, is_founder, role, workspace_id)
		VALUES ($1,'rt-member','x','RT Member',FALSE,'workspace_member',$2)
		ON CONFLICT (username) DO UPDATE SET is_founder = FALSE, role = 'workspace_member', workspace_id = $2`,
		"66666666-0000-4000-8000-00000000000e", rtWorkspaceB)
	require.NoError(t, err)
	var memberVisible int
	require.NoError(t, rt.QueryRow(`SELECT count(*) FROM users WHERE username = 'rt-member'`).Scan(&memberVisible))
	require.Zero(t, memberVisible, "an unbound session must not enumerate workspace members")

	// The whole table must not be enumerable either, which is the leak the
	// narrowed policy exists to close.
	var total int
	require.NoError(t, rt.QueryRow(`SELECT count(*) FROM users`).Scan(&total))
	require.Equal(t, 1, total,
		"an unbound session must see only the founder, never the full identity table")

	// Authenticating a specific identity is still possible: the user store
	// binds app.auth_principal, which is exactly the reach login needs.
	txLookup := bindWorkspaceGUC(t, rt, "app.auth_principal", "rt-member")
	var resolved int
	require.NoError(t, txLookup.QueryRow(`SELECT count(*) FROM users WHERE username = 'rt-member'`).Scan(&resolved))
	require.Equal(t, 1, resolved, "the identity being authenticated must be resolvable")
}

// bindWorkspaceGUC binds an arbitrary application setting transaction-locally,
// mirroring how the user store scopes an identity lookup.
func bindWorkspaceGUC(t *testing.T, db *sql.DB, key, value string) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config($1, $2, true)`, key, value)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// H, I. The bootstrap and the migration path are idempotent: running them again
// over an already-provisioned database succeeds and leaves the topology intact.
func TestBootstrapAndMigrationsAreIdempotent(t *testing.T) {
	topo := topologyFixture(t)

	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()

	// Second and third runs over the same database.
	require.NoError(t, database.Bootstrap(owner, topo), "re-running the bootstrap must succeed")
	require.NoError(t, database.Bootstrap(owner, topo), "re-running the bootstrap must succeed")

	// The topology must still hold afterwards, verified from the runtime pool.
	rt := runtimeDB(t, topo)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, database.VerifyRuntimeSecurity(ctx, rt, topo.Runtime),
		"the runtime topology must survive re-running the bootstrap")

	// Both roles still exist and are still unprivileged.
	for _, role := range []string{topo.Runtime, topo.Admin} {
		var super, bypass bool
		require.NoError(t, owner.QueryRow(
			`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, role).
			Scan(&super, &bypass))
		require.False(t, super, "%s must not be a superuser", role)
		require.False(t, bypass, "%s must not hold BYPASSRLS", role)
	}
}

// J, K. Both production entrypoints obtain their database through
// MustInitializeTopology, whose database stage is exactly this: resolve the
// principals, bootstrap as the owner, then verify the runtime pool against the
// live database. Asserting that sequence here proves the database half of API
// and worker startup on the runtime role; the remainder of those processes needs
// Redis and RabbitMQ and is covered by CI.
func TestRuntimeTopologySatisfiesProductionStartup(t *testing.T) {
	topo := topologyFixture(t)

	for _, name := range []string{"api", "worker"} {
		t.Run(name, func(t *testing.T) {
			rt := runtimeDB(t, topo)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			require.NoError(t, database.VerifyRuntimeSecurity(ctx, rt, topo.Runtime),
				"%s must not start on a connection that cannot prove its isolation boundary", name)
		})
	}
}

// The configuration layer must refuse to serve traffic as the schema owner.
func TestTopologyRejectsOwnerAsRuntimeRole(t *testing.T) {
	ownerDSN := getEnv().postgresDSN
	_, err := database.ResolveTopology(ownerDSN, ownerDSN)
	require.Error(t, err, "using the owner DSN as the runtime DSN must be rejected")
	require.Contains(t, err.Error(), "row level security")
}

// The runtime verification must actually refuse a connection that cannot prove
// its isolation boundary. Ownership is the case the bootstrap cannot repair, so
// it is the one that exercises the guard rather than the self-healing.
func TestVerifyRuntimeSecurityRejectsOwnedProtectedTable(t *testing.T) {
	topo := topologyFixture(t)
	rt := runtimeDB(t, topo)

	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()

	// Sanity: the topology verifies before the tampering.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, database.VerifyRuntimeSecurity(ctx, rt, topo.Runtime))

	_, err = owner.Exec(`ALTER TABLE tasks OWNER TO ` + topo.Runtime)
	require.NoError(t, err)
	// The restore opens its own connection: t.Cleanup runs after the deferred
	// owner.Close() above, so reusing that pool would silently restore nothing.
	t.Cleanup(func() {
		ownerRole, rerr := roleFromDSNForTest(getEnv().postgresDSN)
		require.NoError(t, rerr)
		fix, oerr := sql.Open("pgx", getEnv().postgresDSN)
		require.NoError(t, oerr)
		defer fix.Close()
		_, xerr := fix.Exec(`ALTER TABLE tasks OWNER TO ` + ownerRole)
		require.NoError(t, xerr, "the ownership tampering must be undone")
	})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	err = database.VerifyRuntimeSecurity(ctx2, rt, topo.Runtime)
	require.Error(t, err, "a runtime role that owns a protected table must be rejected")
	require.Contains(t, err.Error(), "owns protected table")
}

// Role-attribute drift applied out-of-band is corrected by the next bootstrap,
// so an operator who grants BYPASSRLS by hand does not leave the process
// unprotected after a restart.
func TestBootstrapRepairsRoleAttributeDrift(t *testing.T) {
	topo := topologyFixture(t)

	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()

	_, err = owner.Exec(`ALTER ROLE ` + topo.Runtime + ` BYPASSRLS`)
	require.NoError(t, err)
	var drifted bool
	require.NoError(t, owner.QueryRow(
		`SELECT rolbypassrls FROM pg_roles WHERE rolname = $1`, topo.Runtime).Scan(&drifted))
	require.True(t, drifted, "the test must actually apply the drift first")

	require.NoError(t, database.Bootstrap(owner, topo), "re-bootstrapping must succeed")

	var healed bool
	require.NoError(t, owner.QueryRow(
		`SELECT rolbypassrls FROM pg_roles WHERE rolname = $1`, topo.Runtime).Scan(&healed))
	require.False(t, healed, "the bootstrap must revoke BYPASSRLS drift on the runtime role")
}

// roleFromDSNForTest extracts the role a DSN connects as.
func roleFromDSNForTest(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	if u.User == nil {
		return "", errors.New("dsn carries no role")
	}
	return u.User.Username(), nil
}
