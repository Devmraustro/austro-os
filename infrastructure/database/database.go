package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"austro-os/internal/config"
	logger "austro-os/internal/log"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Handles are the database connections a running process holds, and holding
// them as separate named values is what makes the security topology explicit at
// every call site: request traffic goes through Runtime, the two
// organization-level operations go through Admin, and nothing holds Owner
// because it is closed before the process serves a request.
type Handles struct {
	// Runtime is the unprivileged, workspace-isolated connection pool. Every
	// request-scoped store must use this one.
	Runtime *sql.DB
	// Admin serves the narrow organization-level operations that a
	// workspace-scoped policy cannot express: founder workspace
	// listing/creation, and appending to and verifying the audit chain. It is
	// not a superuser and has no BYPASSRLS; its reach comes from explicit
	// policies naming the role.
	Admin *sql.DB
	// Topology records the resolved principals, for logging and diagnostics.
	Topology *Topology
}

// Initialize bootstraps the schema and returns the unprivileged runtime handle.
// It exists for callers that only need one pool; MustInitializeTopology is the
// form that exposes the administrative handle as well.
func Initialize(cfg *config.Config) *sql.DB {
	return MustInitializeTopology(cfg).Runtime
}

// MustInitializeTopology applies the schema as the owner principal, provisions
// the runtime and admin principals, and returns pools connected as those
// principals. The owner pool is closed before returning, so no code path in the
// process can reach the schema owner credential after startup.
//
// Before anything is returned the runtime pool is verified against the live
// database (VerifyRuntimeSecurity). That check is what turns the topology from
// an intention into a property: it fails startup if the role the process
// actually connected as is a superuser, holds BYPASSRLS, owns a protected
// table, is missing a forced policy, or can in fact see another workspace's
// rows.
func MustInitializeTopology(cfg *config.Config) *Handles {
	topo, err := ResolveTopology(cfg.PostgresDSN, cfg.PostgresRuntimeDSN)
	if err != nil {
		logger.NewEntry("database-topology-invalid").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	owner := openOrFail(topo.OwnerDSN, "database-open-failed")
	defer owner.Close()

	if err := Bootstrap(owner, topo); err != nil {
		logger.NewEntry("database-bootstrap-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	runtime := openOrFail(topo.RuntimeDSN, "database-runtime-open-failed")
	admin := openOrFail(topo.AdminDSN, "database-admin-open-failed")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := VerifyRuntimeSecurity(ctx, runtime, topo.Runtime); err != nil {
		logger.NewEntry("database-runtime-security-failed").SetLevel("error").WithError(err).Log()
		_ = runtime.Close()
		_ = admin.Close()
		os.Exit(1)
	}

	logger.NewEntry("database-topology-ready").
		With("runtime_role", topo.Runtime).
		With("admin_role", topo.Admin).
		With("rls_tables_forced", len(rlsTables())).
		Log()

	return &Handles{Runtime: runtime, Admin: admin, Topology: topo}
}

// Bootstrap applies the schema, the row level security configuration and the
// role topology using an already-open owner connection.
//
// It is the single definition of the provisioning sequence: MustInitializeTopology
// runs it at startup, and the runtime tests run the same function against a real
// PostgreSQL, so the topology the tests prove is the topology the process
// builds. It returns an error rather than exiting so a failure is a test
// failure instead of a killed binary.
func Bootstrap(owner *sql.DB, topo *Topology) error {
	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"extensions", func() error { return createExtensions(owner) }},
		{"tables", func() error { return createTables(owner) }},
		{"migrate-users", func() error { return migrateUsers(owner) }},
		{"migrate-audit-events", func() error { return migrateAuditEvents(owner) }},
		{"enable-rls", func() error { return enableRLS(owner) }},
		// Roles before policies: CREATE POLICY ... TO <role> requires the role
		// to exist, so provisioning them afterwards fails the policy step.
		{"roles", func() error { return provisionRoles(owner, topo) }},
		{"table-ownership", func() error { return assertOwnership(owner, topo) }},
		{"policies", func() error { return setupRLSPolicies(owner, topo.Admin) }},
		{"force-rls", func() error { return forceRLS(owner) }},
	} {
		if err := step.fn(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	// Vector indexes are a performance control, so a failure is logged and
	// bootstrap proceeds: refusing to serve because an index could not be built
	// is the worse outcome.
	createVectorIndexes(owner)
	return nil
}

// openOrFail dials and pings a DSN, exiting on failure. The ping matters:
// sql.Open only validates the DSN's shape, so without it a bad credential
// surfaces as a request-time error instead of a startup one.
func openOrFail(dsn, event string) *sql.DB {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		logger.NewEntry(event).SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		logger.NewEntry(event).SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
	return db
}

// createVectorIndexes provisions the approximate nearest-neighbour indexes the
// similarity searches rely on. Both memory retrieval and knowledge search order
// by the cosine distance operator (<=>), which without an index is an exact
// scan of every row in the workspace.
//
// These are a performance control rather than a correctness or security one, so
// a failure is logged loudly and startup continues: refusing to serve because
// an index could not be built would be the worse outcome. HNSW is used because,
// unlike IVFFlat, it needs no training rows and so can be built against an
// empty table at first boot.
func createVectorIndexes(db *sql.DB) {
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_memory_embeddings_embedding
			ON memory_embeddings USING hnsw (embedding vector_cosine_ops)`,
		`CREATE INDEX IF NOT EXISTS idx_knowledge_documents_embedding
			ON knowledge_documents USING hnsw (embedding vector_cosine_ops)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			logger.NewEntry("vector-index-status").SetLevel("warn").WithError(err).Log()
		}
	}
}

func createExtensions(db *sql.DB) error {
	// pgvector is required, not optional: knowledge_documents and
	// memory_embeddings declare vector columns, so without the extension the
	// very next step cannot create the schema. Failing here gives a clear
	// reason instead of a confusing CREATE TABLE error.
	if _, err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("create vector extension: %w", err)
	}
	// pgcrypto is best-effort. gen_random_uuid() is built into PostgreSQL 13+,
	// so a cluster that cannot install the extension still has everything the
	// schema needs; only warn.
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS "pgcrypto"`); err != nil {
		logger.NewEntry("pgcrypto-extension-status").SetLevel("warn").WithError(err).Log()
	}
	return nil
}

func createTables(db *sql.DB) error {
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
		seq BIGINT GENERATED BY DEFAULT AS IDENTITY UNIQUE,
		workspace_id UUID,
		timestamp TIMESTAMP DEFAULT NOW(),
		timestamp_canonical TEXT,
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
	CREATE INDEX IF NOT EXISTS idx_audit_events_workspace ON audit_events(workspace_id);

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

	CREATE TABLE IF NOT EXISTS memory_embeddings (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		memory_id TEXT NOT NULL,
		content TEXT NOT NULL,
		embedding vector(10)
	);

	CREATE UNIQUE INDEX IF NOT EXISTS uq_memory_embeddings_memory_id ON memory_embeddings(memory_id);
	CREATE INDEX IF NOT EXISTS idx_memory_embeddings_workspace ON memory_embeddings(workspace_id);

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
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}
	return nil
}

// migrateUsers upgrades the users table to the workspace-scoped RBAC model:
// a role column (founder | workspace_admin | workspace_member), a founder
// backfill, and CHECK constraints enforcing the invariant that a founder has
// no workspace and any non-founder always does. The constraints are the same
// guarantees the authorization layer assumes, so the database enforces them
// even if an insert bypasses the service.
func migrateUsers(db *sql.DB) error {
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
	if _, err := db.Exec(migration); err != nil {
		// A constraint add can only fail if pre-existing rows violate the new
		// invariant; surface it loudly rather than silently weakening the model.
		return fmt.Errorf("migrate users: %w", err)
	}
	return nil
}

// rlsTables are the tables whose rows are workspace- or identity-scoped. Row
// level security on these is a security control, not a best-effort extra, so a
// failure to enable it is fatal: continuing would leave the process serving
// requests with an isolation boundary it believes it has.
//
// founders is deliberately absent: it is organization-level and its policy is
// USING (true), so enabling row level security there would change nothing.
//
// audit_events IS included. Its rows carry the workspace they were recorded
// for, and an audit trail that any tenant could read across the boundary would
// leak the existence and timing of other tenants' security-relevant activity.
// Organization-level events (login, bootstrap, refresh replay) have no
// workspace and are visible to every workspace-scoped reader by design; the
// whole chain is readable only by the administrative role, which is what
// verification runs as.
func rlsTables() []string {
	return []string{"workspaces", "departments", "teams", "ai_employees", "ceos", "tasks", "knowledge_documents", "memory_embeddings", "publications", "pipelines", "users", "audit_events"}
}

// migrateAuditEvents upgrades audit_events for persistent, workspace-scoped
// audit records: a workspace column for scoping, and a monotonic sequence that
// gives the hash chain a total order to be read back in. Without an ordering
// column the only way to reconstruct the chain is to walk hash_chain_parent
// from the genesis row, which cannot be indexed into a single ordered scan.
//
// timestamp_canonical holds the event timestamp rendered exactly as it was
// bound into the chain hash. It is not a convenience duplicate: PostgreSQL
// timestamp columns carry microsecond precision while the chain pre-image binds
// RFC 3339 with nanoseconds, so reading the timestamp back from the column
// alone would recompute a different hash and report an intact chain as
// tampered. Verification parses the canonical text instead.
//
// It is idempotent and runs on every boot, so a database created before
// persistent audit existed is upgraded rather than silently left without the
// columns the writer needs.
func migrateAuditEvents(db *sql.DB) error {
	migration := `
	ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS seq BIGINT GENERATED BY DEFAULT AS IDENTITY;
	ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS workspace_id UUID;
	ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS timestamp_canonical TEXT;
	CREATE UNIQUE INDEX IF NOT EXISTS uq_audit_events_seq ON audit_events(seq);
	CREATE INDEX IF NOT EXISTS idx_audit_events_workspace ON audit_events(workspace_id);
	`
	if _, err := db.Exec(migration); err != nil {
		return fmt.Errorf("migrate audit_events: %w", err)
	}
	return nil
}

func enableRLS(db *sql.DB) error {
	for _, table := range rlsTables() {
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", table)); err != nil {
			return fmt.Errorf("enable row level security on %s: %w", table, err)
		}
	}
	return nil
}

func setupRLSPolicies(db *sql.DB, adminRole string) error {
	// The administrative role name is published as a session setting rather
	// than interpolated into the DDL text, so a role name can never break out
	// of the statement. quote_ident inside the DO block does the quoting.
	// The administrative role name is interpolated rather than bound. A role
	// name cannot be a bind parameter inside a DO block, and routing it through
	// a session setting is not safe here either: db.Exec may run each statement
	// on a different pooled connection, so a setting applied by one statement
	// is absent from the next and the concatenation silently yields NULL.
	//
	// Direct interpolation is safe because the name is validated against a
	// pattern that admits no quote, space or metacharacter. The check is
	// repeated here rather than trusted from the caller, because this function
	// builds DDL text.
	if !validRole.MatchString(adminRole) {
		return fmt.Errorf("admin role name %q is not a supported identifier", adminRole)
	}
	policies := fmt.Sprintf(`
	DO $$
	BEGIN
		-- Workspaces: direct match on id
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'workspaces'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON workspaces
				USING (id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Founders: organization-level, not workspace-scoped
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'founder_org_policy' AND polrelid = 'founders'::regclass) THEN
			EXECUTE 'CREATE POLICY founder_org_policy ON founders
				USING (true)';
		END IF;

		-- CEOS: match workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'ceos'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON ceos
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Departments: match workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'departments'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON departments
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Teams: traverse through departments
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'teams'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON teams
				USING (department_id IN (SELECT id FROM departments WHERE workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID))';
		END IF;

		-- AI Employees: traverse through teams -> departments
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'ai_employees'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON ai_employees
				USING (team_id IN (SELECT t.id FROM teams t JOIN departments d ON t.department_id = d.id WHERE d.workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID))';
		END IF;

		-- Tasks: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'tasks'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON tasks
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Knowledge documents: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'knowledge_documents'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON knowledge_documents
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Memory embeddings: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'memory_embeddings'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON memory_embeddings
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Publications: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'publications'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON publications
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Pipelines: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'pipelines'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON pipelines
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Users: founder identity is organization-level (visible without a
		-- workspace context so login/bootstrap can resolve it); non-founder
		-- identities are workspace-scoped. When a workspace context is set on
		-- the connection, a restricted role sees only its own workspace's users
		-- plus the founder.
		--
		-- The "no workspace context" branch must go through COALESCE.
		-- current_setting(name, true) returns NULL, not the empty string,
		-- when the setting is absent, so comparing it to '' is never true and
		-- the branch was dead: an unbound session matched no rows at all,
		-- which is why login and bootstrap only worked because the runtime
		-- role owns the table. Drop-and-create rather than IF NOT EXISTS so a
		-- database provisioned with the earlier definition is corrected.
		-- Organization-level operations run as a distinct administrative
		-- role, and the wider policy is attached TO that role by name. The
		-- runtime role is not a member of it, so no amount of session state
		-- set from application code can widen the runtime role's view: the
		-- escalation lives in the credential, not in a setting a code path
		-- could leave behind on a pooled connection.
		EXECUTE 'DROP POLICY IF EXISTS org_admin_policy ON workspaces';
		EXECUTE 'CREATE POLICY org_admin_policy ON workspaces TO %s USING (true)';

		-- Audit rows are workspace-scoped for ordinary readers, with
		-- organization-level events (no workspace) visible to everyone so
		-- authentication history stays reachable from the session it belongs
		-- to.
		EXECUTE 'DROP POLICY IF EXISTS audit_workspace_policy ON audit_events';
		EXECUTE 'CREATE POLICY audit_workspace_policy ON audit_events
			USING (workspace_id IS NULL
			       OR workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';

		-- Chain verification needs the complete chain, so the administrative
		-- role gets an organization-scoped read of the audit table.
		EXECUTE 'DROP POLICY IF EXISTS audit_org_policy ON audit_events';
		EXECUTE 'CREATE POLICY audit_org_policy ON audit_events TO %s USING (true)';

		-- Users: identity resolution is organization-level, because login has
		-- to find an identity before any workspace is known. Earlier revisions
		-- expressed that as "no workspace context bound implies everything is
		-- visible", which is far broader than authentication needs: any unbound
		-- session holding the runtime credential could enumerate every identity
		-- in every tenant, password hashes included.
		--
		-- The unbound reach is therefore narrowed to the single identity the
		-- caller is authenticating, named by app.auth_principal and bound
		-- transaction-locally by the user store. An unbound session with no
		-- principal bound sees only founders, which is what the bootstrap
		-- "already initialized" check needs and nothing more.
		EXECUTE 'DROP POLICY IF EXISTS user_scope_policy ON users';
		EXECUTE 'CREATE POLICY user_scope_policy ON users
			USING (is_founder
			       OR workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID
			       OR username = current_setting(''app.auth_principal'', true)
			       OR id::text = current_setting(''app.auth_principal'', true))';
	END$$;
	`, adminRole, adminRole)
	if _, err := db.Exec(policies); err != nil {
		return fmt.Errorf("create row level security policies: %w", err)
	}
	return nil
}
