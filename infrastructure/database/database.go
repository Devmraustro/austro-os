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
	migrateUsers(db)
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

	CREATE TABLE IF NOT EXISTS workspaces (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS ceos (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		created_at TIMESTAMP DEFAULT NOW()
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

	CREATE TABLE IF NOT EXISTS tasks (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		title TEXT NOT NULL,
		description TEXT,
		status TEXT NOT NULL DEFAULT 'backlog',
		priority TEXT NOT NULL DEFAULT 'normal',
		assignee_type TEXT NOT NULL DEFAULT 'ai_employee',
		assignee_id UUID,
		deadline TIMESTAMP,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_workspace ON tasks(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
	CREATE INDEX IF NOT EXISTS idx_tasks_workspace_status ON tasks(workspace_id, status);

	CREATE TABLE IF NOT EXISTS knowledge_documents (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		title TEXT NOT NULL,
		content TEXT NOT NULL,
		embedding vector(10),
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_knowledge_workspace ON knowledge_documents(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_knowledge_kind ON knowledge_documents(workspace_id, kind);

	CREATE TABLE IF NOT EXISTS publications (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		goal_id UUID,
		task_id UUID,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		platform TEXT NOT NULL,
		status TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		approved_by TEXT,
		approved_at TIMESTAMP,
		rejected_by TEXT,
		rejected_at TIMESTAMP,
		published_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_publications_workspace ON publications(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_publications_status ON publications(workspace_id, status);

	CREATE TABLE IF NOT EXISTS pipelines (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		goal_id UUID,
		stage TEXT NOT NULL,
		status TEXT NOT NULL,
		task_id UUID,
		publication_id UUID,
		trace_id TEXT,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_pipelines_workspace ON pipelines(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_pipelines_status ON pipelines(workspace_id, status);

	CREATE TABLE IF NOT EXISTS users (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		username TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		display_name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '',
		is_founder BOOLEAN NOT NULL DEFAULT FALSE,
		workspace_id UUID REFERENCES workspaces(id) ON DELETE SET NULL,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW(),
		CONSTRAINT users_username_unique UNIQUE (username)
	);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_users_single_founder
		ON users ((TRUE)) WHERE is_founder = TRUE;
	CREATE INDEX IF NOT EXISTS idx_users_workspace ON users(workspace_id);
`
	_, err := db.Exec(schema)
	if err != nil {
		logger.NewEntry("database-create-tables-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
}

// migrateUsers upgrades the users table to the workspace-scoped RBAC model:
// a role column (founder | workspace_admin | workspace_member), a founder
// backfill, and CHECK constraints enforcing the invariant that a founder has
// no workspace and any non-founder always does. The constraints are the same
// guarantees the authorization layer assumes, so the database enforces them
// even if an insert bypasses the service.
func migrateUsers(db *sql.DB) {
	migration := `
	ALTER TABLE users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'workspace_member';
	UPDATE users SET role = 'founder' WHERE is_founder = TRUE AND role = 'workspace_member';

	DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_role_valid') THEN
			ALTER TABLE users ADD CONSTRAINT users_role_valid
				CHECK (role IN ('founder', 'workspace_admin', 'workspace_member'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_founder_role_matches') THEN
			ALTER TABLE users ADD CONSTRAINT users_founder_role_matches
				CHECK (is_founder = (role = 'founder'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_founder_no_workspace') THEN
			ALTER TABLE users ADD CONSTRAINT users_founder_no_workspace
				CHECK (NOT is_founder OR workspace_id IS NULL);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'users_nonfounder_workspace_required') THEN
			ALTER TABLE users ADD CONSTRAINT users_nonfounder_workspace_required
				CHECK (is_founder OR workspace_id IS NOT NULL);
		END IF;
	END$$;
	`
	_, err := db.Exec(migration)
	if err != nil {
		// A constraint add can only fail if pre-existing rows violate the new
		// invariant; surface it loudly rather than silently weakening the model.
		logger.NewEntry("database-migrate-users-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
}

func enableRLS(db *sql.DB) {
	tables := []string{"workspaces", "departments", "teams", "ai_employees", "ceos", "tasks", "knowledge_documents", "publications", "pipelines", "users"}
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

		-- Tasks: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'tasks'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON tasks
				USING (workspace_id = current_setting(''app.current_workspace'', true)::UUID)';
		END IF;

		-- Knowledge documents: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'knowledge_documents'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON knowledge_documents
				USING (workspace_id = current_setting(''app.current_workspace'', true)::UUID)';
		END IF;

		-- Publications: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'publications'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON publications
				USING (workspace_id = current_setting(''app.current_workspace'', true)::UUID)';
		END IF;

		-- Pipelines: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'pipelines'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON pipelines
				USING (workspace_id = current_setting(''app.current_workspace'', true)::UUID)';
		END IF;

		-- Users: founder identity is organization-level (visible without a
		-- workspace context so login/bootstrap can resolve it); non-founder
		-- identities are workspace-scoped. When a workspace context is set on
		-- the connection, a restricted role sees only its own workspace's users
		-- plus the founder.
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'user_scope_policy' AND polrelid = 'users'::regclass) THEN
			EXECUTE 'CREATE POLICY user_scope_policy ON users
				USING (current_setting(''app.current_workspace'', true) = ''''
				       OR is_founder
				       OR workspace_id = current_setting(''app.current_workspace'', true)::UUID)';
		END IF;
	END$$;
	`
	_, err := db.Exec(policies)
	if err != nil {
		logger.NewEntry("rls-policies-setup").SetLevel("warn").WithError(err).Log()
	}
}
