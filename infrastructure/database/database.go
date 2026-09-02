package database

import (
	"database/sql"
	"fmt"
	"os"

	"austro-os/internal/config"
	logger "austro-os/internal/log"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func Initialize(cfg *config.Config) *sql.DB {
	db, err := sql.Open("pgx", cfg.PostgresDSN)
	if err != nil {
		logger.NewEntry("database-open-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	if err := db.Ping(); err != nil {
		logger.NewEntry("database-ping-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	createExtensions(db)
	createTables(db)
	enableRLS(db)
	setupRLSPolicies(db)

	return db
}

func createExtensions(db *sql.DB) {
	_, err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector")
	if err != nil {
		logger.NewEntry("vector-extension-status").SetLevel("warn").WithError(err).Log()
	}
	_, err = db.Exec(`CREATE EXTENSION IF NOT EXISTS "pgcrypto"`)
	if err != nil {
		logger.NewEntry("pgcrypto-extension-status").SetLevel("warn").WithError(err).Log()
	}
}

func createTables(db *sql.DB) {
	schema := `
	CREATE TABLE IF NOT EXISTS founders (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS ceos (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS workspaces (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS departments (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS teams (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		department_id UUID NOT NULL REFERENCES departments(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS ai_employees (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		role TEXT NOT NULL,
		capabilities TEXT[] DEFAULT '{}',
		permissions JSONB DEFAULT '{}',
		memory_id UUID,
		knowledge_access JSONB DEFAULT '{}',
		current_task_id UUID,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_workspaces_name ON workspaces(name);
	CREATE INDEX IF NOT EXISTS idx_ceos_workspace ON ceos(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_departments_workspace ON departments(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_teams_department ON teams(department_id);
	CREATE INDEX IF NOT EXISTS idx_ai_employees_team ON ai_employees(team_id);
	CREATE INDEX IF NOT EXISTS idx_ai_employees_current_task ON ai_employees(current_task_id);

	CREATE TABLE IF NOT EXISTS audit_events (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		event_id UUID NOT NULL UNIQUE,
		timestamp TIMESTAMP DEFAULT NOW(),
		trace_id UUID,
		span_id UUID,
		actor_type TEXT NOT NULL,
		actor_id UUID,
		target_type TEXT NOT NULL,
		target_id UUID,
		event_type TEXT NOT NULL,
		outcome TEXT NOT NULL,
		outcome_details JSONB,
		permissions_checked JSONB,
		constitutional_principle TEXT NOT NULL,
		digital_signature JSONB,
		hash_chain_parent UUID,
		hash_chain_value BYTEA,
		genesis BOOLEAN DEFAULT false
	);

	CREATE INDEX IF NOT EXISTS idx_audit_events_event_id ON audit_events(event_id);
	CREATE INDEX IF NOT EXISTS idx_audit_events_actor ON audit_events(actor_type, actor_id);
	CREATE INDEX IF NOT EXISTS idx_audit_events_timestamp ON audit_events(timestamp);
	CREATE INDEX IF NOT EXISTS idx_audit_events_constitutional ON audit_events(constitutional_principle);
`
	_, err := db.Exec(schema)
	if err != nil {
		logger.NewEntry("database-create-tables-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
}

func enableRLS(db *sql.DB) {
	tables := []string{"workspaces", "departments", "teams", "ai_employees", "ceos"}
	for _, table := range tables {
		_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", table))
		if err != nil {
			logger.NewEntry("rls-enable-failed").SetLevel("warn").With("table", table).WithError(err).Log()
		}
	}
}

func setupRLSPolicies(db *sql.DB) {
	policies := `
	DO $$
	BEGIN
		-- Workspaces: direct match on id
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'workspaces'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON workspaces
				USING (id = current_setting(''app.current_workspace'', true)::UUID)';
		END IF;

		-- Founders: organization-level, not workspace-scoped
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'founder_org_policy' AND polrelid = 'founders'::regclass) THEN
			EXECUTE 'CREATE POLICY founder_org_policy ON founders
				USING (true)';
		END IF;

		-- CEOS: match workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'ceos'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON ceos
				USING (workspace_id = current_setting(''app.current_workspace'', true)::UUID)';
		END IF;

		-- Departments: match workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'departments'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON departments
				USING (workspace_id = current_setting(''app.current_workspace'', true)::UUID)';
		END IF;

		-- Teams: traverse through departments
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'teams'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON teams
				USING (department_id IN (SELECT id FROM departments WHERE workspace_id = current_setting(''app.current_workspace'', true)::UUID))';
		END IF;

		-- AI Employees: traverse through teams -> departments
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'ai_employees'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON ai_employees
				USING (team_id IN (SELECT t.id FROM teams t JOIN departments d ON t.department_id = d.id WHERE d.workspace_id = current_setting(''app.current_workspace'', true)::UUID))';
		END IF;
	END$$;
	`
	_, err := db.Exec(policies)
	if err != nil {
		logger.NewEntry("rls-policies-setup").SetLevel("warn").WithError(err).Log()
	}
}
