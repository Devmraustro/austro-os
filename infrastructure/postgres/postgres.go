package postgres

import (
	"database/sql"

	"austro-os/infrastructure/database"
	"austro-os/internal/config"
	logger "austro-os/internal/log"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// SQLDB wraps *sql.DB with AUSTRO OS conventions.
type SQLDB struct {
	*sql.DB
}

// Initialize returns the runtime database handle with the authoritative schema
// applied. The schema, the row level security enablement and the workspace
// policies all live in infrastructure/database; this delegates there so there
// is exactly one definition of the runtime schema instead of a second copy here
// that can silently drift from the one the process actually applies.
func Initialize(cfg *config.Config) *SQLDB {
	// database.Initialize returns the unprivileged runtime pool and has
	// already applied the schema and verified the runtime role against the
	// live database. The policies are not re-asserted here: that would run
	// CREATE POLICY on a role that deliberately owns nothing and has no
	// authority to create them.
	return &SQLDB{DB: database.Initialize(cfg)}
}

// EnableRLSPolicies asserts the workspace isolation policies on a connection
// that is already open. It is idempotent, and it requires an owner-privileged
// connection: CREATE POLICY is not available to the application runtime role,
// which is the point of the split. Production bootstrap goes through
// infrastructure/database, which owns the authoritative policy definitions;
// this exists for callers that provision a schema outside that path.
//
// A failure is logged rather than fatal here because the caller may hold a
// handle that was never meant to own the schema. infrastructure/database's own
// bootstrap is fatal on the same failure.
//
// The expressions mirror infrastructure/database: workspaces is matched on its
// own id (it has no workspace_id column of its own), and teams/ai_employees
// reach their workspace through departments. An earlier revision of this block
// referenced columns that do not exist (workspaces.workspace_id,
// teams.department_workspace_id, ai_employees.team_department_workspace_id), so
// the whole DO block failed and was swallowed as a warning.
func EnableRLSPolicies(db *sql.DB) {
	_, err := db.Exec(`DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'workspaces'::regclass) THEN
			CREATE POLICY workspace_isolation_policy ON workspaces
				USING (id = current_setting('app.current_workspace', true)::UUID);
			CREATE POLICY workspace_isolation_policy ON ceos
				USING (workspace_id = current_setting('app.current_workspace', true)::UUID);
			CREATE POLICY workspace_isolation_policy ON departments
				USING (workspace_id = current_setting('app.current_workspace', true)::UUID);
			CREATE POLICY workspace_isolation_policy ON teams
				USING (department_id IN (SELECT id FROM departments WHERE workspace_id = current_setting('app.current_workspace', true)::UUID));
			CREATE POLICY workspace_isolation_policy ON ai_employees
				USING (team_id IN (SELECT t.id FROM teams t JOIN departments d ON t.department_id = d.id WHERE d.workspace_id = current_setting('app.current_workspace', true)::UUID));
			CREATE POLICY workspace_isolation_policy ON tasks
				USING (workspace_id = current_setting('app.current_workspace', true)::UUID);
			CREATE POLICY workspace_isolation_policy ON knowledge_documents
				USING (workspace_id = current_setting('app.current_workspace', true)::UUID);
			CREATE POLICY workspace_isolation_policy ON publications
				USING (workspace_id = current_setting('app.current_workspace', true)::UUID);
			CREATE POLICY workspace_isolation_policy ON pipelines
				USING (workspace_id = current_setting('app.current_workspace', true)::UUID);
		END IF;
	END$$;`)
	if err != nil {
		logger.NewEntry("rls-policies-note").SetLevel("warn").WithError(err).Log()
	}
}
