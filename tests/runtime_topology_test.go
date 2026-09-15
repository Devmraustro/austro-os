package austro_os_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"austro-os/infrastructure/database"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// The tests in this file exercise the security topology the application
// actually runs on, against a real PostgreSQL.
//
// Three principals are involved and the distinction is the entire point:
//
//	owner    AUSTRO_POSTGRES_DSN          superuser in this environment; owns
//	                                      the tables; used only to provision.
//	runtime  topo.RuntimeDSN              the role the API and worker serve on.
//	                                      Not a superuser, no BYPASSRLS, owns
//	                                      nothing.
//	admin    topo.AdminDSN                organization-level operations and the
//	                                      audit chain.
//
// Assertions about the runtime role are made FROM the runtime connection, never
// from the privileged one. A check that asked the superuser "does austro_app
// bypass RLS?" would still pass if the application were secretly connecting as
// the superuser, which is precisely the defect these tests exist to catch.

// topologyFixture provisions the schema and roles on the test database using
// the production bootstrap, then returns the resolved principals.
func topologyFixture(t *testing.T) *database.Topology {
	t.Helper()
	ownerDSN := getEnv().postgresDSN
	topo, err := database.ResolveTopology(ownerDSN, os.Getenv("AUSTRO_POSTGRES_RUNTIME_DSN"))
	require.NoError(t, err, "the runtime principal must differ from the schema owner")

	owner, err := sql.Open("pgx", ownerDSN)
	require.NoError(t, err)
	defer owner.Close()
	require.NoError(t, owner.Ping())

	// The same function the process runs at startup, so the topology proven
	// here is the topology that gets built in production.
	require.NoError(t, database.Bootstrap(owner, topo))
	return topo
}

// runtimeDB returns a pool connected as the application runtime role.
func runtimeDB(t *testing.T, topo *database.Topology) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", topo.RuntimeDSN)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx), "the runtime role must be able to connect")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// adminDB returns a pool connected as the administrative role.
func adminDB(t *testing.T, topo *database.Topology) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", topo.AdminDSN)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx), "the admin role must be able to connect")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// protectedTables mirrors infrastructure/database's protected set. It is
// duplicated deliberately: a test that read the list from the code under test
// would pass if the code dropped a table.
var protectedTables = []string{
	"workspaces", "departments", "teams", "ai_employees", "ceos", "tasks",
	"knowledge_documents", "memory_embeddings", "publications", "pipelines",
	"users", "audit_events",
}

// seedWorkspace inserts a workspace through the owner connection, which is how
// a deployment or a migration creates tenant rows.
func seedWorkspace(t *testing.T, topo *database.Topology, id, name string) {
	t.Helper()
	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()
	_, err = owner.Exec(`INSERT INTO workspaces (id, name) VALUES ($1,$2)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`, id, name)
	require.NoError(t, err)
}

// bindWorkspace starts a transaction with app.current_workspace bound
// transaction-locally, which is the discipline every production store uses.
func bindWorkspace(t *testing.T, db *sql.DB, workspaceID string) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config('app.current_workspace', $1, true)`, workspaceID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}
