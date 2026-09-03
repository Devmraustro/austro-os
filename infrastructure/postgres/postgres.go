package postgres

import (
	"database/sql"
	"os"

	"austro-os/internal/config"
	logger "austro-os/internal/log"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// SQLDB wraps *sql.DB with AUSTRO OS conventions
type SQLDB struct {
	*sql.DB
}

func Initialize(cfg *config.Config) *SQLDB {
	db, err := sql.Open("pgx", cfg.PostgresDSN)
	if err != nil {
		logger.NewEntry("database-open-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	if err := db.Ping(); err != nil {
		logger.NewEntry("database-ping-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	// ADR-007: No SET row_security = off
	// Use proper RLS policies instead
	enableRLSPolicies(db)

	// Create extensions
	createExtensions(db)

	// Create tables
	createTables(db)

	return &SQLDB{DB: db}
}

func enableRLSPolicies(db *sql.DB) {
	_, err := db.Exec(`DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy') THEN
			CREATE POLICY workspace_isolation_policy ON workspaces
				USING (workspace_id = current_setting('app.current_workspace', true));
			CREATE POLICY workspace_isolation_policy ON ceos
				USING (workspace_id = current_setting('app.current_workspace', true));
			CREATE POLICY workspace_isolation_policy ON departments
				USING (workspace_id = current_setting('app.current_workspace', true));
			CREATE POLICY workspace_isolation_policy ON teams
				USING (department_workspace_id = current_setting('app.current_workspace', true));
			CREATE POLICY workspace_isolation_policy ON ai_employees
				USING (team_department_workspace_id = current_setting('app.current_workspace', true));
			CREATE POLICY workspace_isolation_policy ON tasks
				USING (workspace_id = current_setting('app.current_workspace', true));
			CREATE POLICY workspace_isolation_policy ON knowledge_documents
				USING (workspace_id = current_setting('app.current_workspace', true));
			CREATE POLICY workspace_isolation_policy ON publications
				USING (workspace_id = current_setting('app.current_workspace', true));
		END IF;
	END$$;`)
	if err != nil {
		logger.NewEntry("rls-policies-note").SetLevel("warn").WithError(err).Log()
	}
}

func disableRowSecurityOff(db *sql.DB) {
	// This is absolutely prohibited per ADR-007
	// Workspace isolation must be enforced at the database layer through RLS policies
	logger.NewEntry("CRITICAL: SET row_security = off is absolutely prohibited. Use proper RLS policies instead.").SetLevel("error").Log()
	os.Exit(1)
}

func createExtensions(db *sql.DB) {
	_, err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector")
	if err != nil {
		logger.NewEntry("vector-extension-status").SetLevel("warn").WithError(err).Log()
	}
	_, err = db.Exec("CREATE EXTENSION IF NOT EXISTS \"pgcrypto\"")
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
	`
	_, err := db.Exec(schema)
	if err != nil {
		logger.NewEntry("database-create-tables-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
}

func (db *SQLDB) WithWorkspaceWorkspaceID(wsID string) *workspaceQueryBuilder {
	return &workspaceQueryBuilder{db: db, workspaceID: wsID}
}

type workspaceQueryBuilder struct {
	db        *SQLDB
	workspaceID string
}

func (b *workspaceQueryBuilder) WrapQuery(query string, args ...interface{}) (string, []interface{}) {
	// Prepend workspace filter to every query
	filteredArgs := append([]interface{}{b.workspaceID}, args...)
	return query + " WHERE workspace_id = $1", filteredArgs
}