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
// (or, in pre-provisioned mode, verifies) the runtime and admin principals, and
// returns pools connected as those principals. The owner pool is closed before
// returning, so no code path in the process can reach the schema owner
// credential after startup.
//
// Before anything is returned the runtime pool is verified against the live
// database (VerifyRuntimeSecurity) and the administrative pool is verified the
// same way at its own principal (VerifyAdminRole). Those checks are what turn
// the topology from an intention into a property: startup fails if the role
// the process actually connected as is a superuser, holds BYPASSRLS, owns a
// protected table, is missing a forced policy, or can in fact see another
// workspace's rows.
func MustInitializeTopology(cfg *config.Config) *Handles {
	topo, err := ResolveTopologyWithAdmin(cfg.PostgresDSN, cfg.PostgresRuntimeDSN, cfg.PostgresAdminDSN)
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
	if err := VerifyAdminRole(ctx, admin, topo.Admin); err != nil {
		logger.NewEntry("database-admin-security-failed").SetLevel("error").WithError(err).Log()
		_ = runtime.Close()
		_ = admin.Close()
		os.Exit(1)
	}

	logger.NewEntry("database-topology-ready").
		With("runtime_role", topo.Runtime).
		With("admin_role", topo.Admin).
		With("pre_provisioned_roles", topo.PreProvisioned).
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
		{"migrate-tasks", func() error { return migrateTasks(owner) }},
		{"migrate-knowledge", func() error { return migrateKnowledge(owner) }},
		{"migrate-publications", func() error { return migratePublications(owner) }},
		{"migrate-pipelines", func() error { return migratePipelines(owner) }},
		{"migrate-organization", func() error { return migrateOrganization(owner) }},
		{"migrate-executions", func() error { return migrateExecutions(owner) }},
		// After every table migration, so the columns the composite keys cover
		// are final, and before RLS, so a violation surfaces as a constraint
		// failure here rather than as a policy error at first request.
		{"workspace-integrity", func() error { return enforceWorkspaceIntegrity(owner) }},
		{"enable-rls", func() error { return enableRLS(owner) }},
		// Roles before policies: CREATE POLICY ... TO <role> requires the role
		// to exist, so provisioning them afterwards fails the policy step. In
		// pre-provisioned mode the roles already exist: this step verifies
		// their attributes against the live database and re-asserts the
		// grants, creating and altering nothing.
		{"roles", func() error {
			if topo.PreProvisioned {
				if err := assertPreProvisionedRoles(owner, topo); err != nil {
					return err
				}
				return grantRolePrivileges(owner, topo)
			}
			return provisionRoles(owner, topo)
		}},
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
		idempotency_key TEXT,
		external_reference TEXT,
		failure_reason TEXT,
		approved_by TEXT,
		approved_at TIMESTAMP,
		rejected_by TEXT,
		rejected_at TIMESTAMP,
		published_at TIMESTAMP,
		published_by TEXT,
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
		research_reference TEXT,
		script_reference TEXT,
		review_reference TEXT,
		published_reference TEXT,
		failure_reason TEXT,
		retry_count INTEGER NOT NULL DEFAULT 0,
		idempotency_key TEXT,
		approved_by TEXT,
		approved_at TIMESTAMP,
		version BIGINT NOT NULL DEFAULT 1,
		trace_id TEXT,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_pipelines_workspace ON pipelines(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_pipelines_status ON pipelines(workspace_id, status);
	CREATE INDEX IF NOT EXISTS idx_pipelines_workspace_created ON pipelines(workspace_id, created_at DESC, id DESC);
	CREATE UNIQUE INDEX IF NOT EXISTS uq_pipelines_workspace_idempotency ON pipelines(workspace_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

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

	CREATE TABLE IF NOT EXISTS ai_executions (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		ai_employee_id UUID NOT NULL REFERENCES ai_employees(id) ON DELETE RESTRICT,
		task_id UUID REFERENCES tasks(id) ON DELETE SET NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		attempt INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 3,
		idempotency_key TEXT,
		request_hash TEXT NOT NULL,
		lease_owner TEXT,
		lease_generation BIGINT NOT NULL DEFAULT 0,
		lease_expires_at TIMESTAMP,
		next_retry_at TIMESTAMP,
		last_error_code INTEGER,
		retryable BOOLEAN,
		error_detail TEXT,
		result_json JSONB,
		completed_at TIMESTAMP,
		failed_at TIMESTAMP,
		cancelled_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT NOW(),
		updated_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_ai_executions_workspace ON ai_executions(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_ai_executions_workspace_status ON ai_executions(workspace_id, status);
	CREATE INDEX IF NOT EXISTS idx_ai_executions_employee ON ai_executions(ai_employee_id, status);
	CREATE INDEX IF NOT EXISTS idx_ai_executions_lease ON ai_executions(status, lease_expires_at);
	CREATE INDEX IF NOT EXISTS idx_ai_executions_next_retry ON ai_executions(status, next_retry_at);
	CREATE INDEX IF NOT EXISTS idx_ai_executions_task ON ai_executions(task_id);
	CREATE UNIQUE INDEX IF NOT EXISTS uq_ai_executions_workspace_idempotency
		ON ai_executions(workspace_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

	CREATE TABLE IF NOT EXISTS ai_execution_retries (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		execution_id UUID NOT NULL REFERENCES ai_executions(id) ON DELETE CASCADE,
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		retry_seq INTEGER NOT NULL,
		state TEXT NOT NULL DEFAULT 'pending',
		outcome TEXT,
		worker_id TEXT,
		generation BIGINT NOT NULL DEFAULT 0,
		next_retry_at TIMESTAMP,
		claimed_at TIMESTAMP,
		dispatched_at TIMESTAMP,
		completed_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_ai_execution_retries_execution ON ai_execution_retries(execution_id);
	CREATE INDEX IF NOT EXISTS idx_ai_execution_retries_workspace ON ai_execution_retries(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_ai_execution_retries_state ON ai_execution_retries(state, next_retry_at);

	CREATE TABLE IF NOT EXISTS retry_dispatch_log (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		retry_id UUID NOT NULL REFERENCES ai_execution_retries(id) ON DELETE CASCADE,
		worker_id TEXT NOT NULL,
		generation BIGINT NOT NULL,
		outcome TEXT NOT NULL,
		recorded_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_retry_dispatch_log_retry ON retry_dispatch_log(retry_id);
	CREATE INDEX IF NOT EXISTS idx_retry_dispatch_log_workspace ON retry_dispatch_log(workspace_id);

	CREATE TABLE IF NOT EXISTS ai_execution_dlq (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		execution_id UUID NOT NULL REFERENCES ai_executions(id) ON DELETE CASCADE,
		attempt INTEGER NOT NULL DEFAULT 0,
		error_code INTEGER NOT NULL,
		retryable BOOLEAN NOT NULL,
		error_detail TEXT,
		contexts_snapshot JSONB,
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_ai_execution_dlq_workspace ON ai_execution_dlq(workspace_id);
	CREATE INDEX IF NOT EXISTS idx_ai_execution_dlq_execution ON ai_execution_dlq(execution_id);

	CREATE TABLE IF NOT EXISTS ai_tool_calls (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		execution_id UUID NOT NULL REFERENCES ai_executions(id) ON DELETE CASCADE,
		tool_name TEXT NOT NULL,
		arguments JSONB,
		status TEXT NOT NULL,
		result JSONB,
		error_detail TEXT,
		duration_ms BIGINT NOT NULL DEFAULT 0,
		created_at TIMESTAMP DEFAULT NOW(),
		completed_at TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_ai_tool_calls_execution ON ai_tool_calls(execution_id);
	CREATE INDEX IF NOT EXISTS idx_ai_tool_calls_workspace ON ai_tool_calls(workspace_id);

	CREATE TABLE IF NOT EXISTS execution_contexts (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
		execution_id UUID NOT NULL REFERENCES ai_executions(id) ON DELETE CASCADE,
		task_title TEXT,
		task_description TEXT,
		ai_employee_name TEXT NOT NULL,
		ai_employee_role TEXT NOT NULL,
		team_name TEXT,
		department_name TEXT,
		capabilities TEXT[] NOT NULL DEFAULT '{}',
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_contexts_execution ON execution_contexts(execution_id);
	CREATE INDEX IF NOT EXISTS idx_execution_contexts_workspace ON execution_contexts(workspace_id);

	-- Process liveness, written by the dispatcher and the worker.
	--
	-- This exists because a split deployment creates a failure one process
	-- cannot see: when the API delegates the sweep to a companion on another
	-- host, the API can start, serve traffic, and answer its own readiness
	-- check while no dispatcher exists anywhere. Nothing inside the API
	-- process can observe that. The only way for the dependency to become
	-- visible is for the companion to leave evidence that it is running, in
	-- the one datastore all units already share.
	--
	-- Deliberately NOT row level security. The invariant "is this process
	-- alive" is infrastructure state, not workspace tenant data: it has no
	-- workspace owner, and the API must be able to read the dispatcher's row
	-- regardless of which workspace is being served. RLS would require a
	-- fictitious owner and a policy that can only be bypassed, so the
	-- privilege is granted explicitly and the table holds no tenant content.
	-- It carries no payload beyond a role name, an opaque instance id, and a
	-- timestamp.
	CREATE TABLE IF NOT EXISTS process_heartbeats (
		role TEXT NOT NULL,
		instance_id TEXT NOT NULL,
		started_at TIMESTAMP NOT NULL DEFAULT NOW(),
		last_seen_at TIMESTAMP NOT NULL DEFAULT NOW(),
		PRIMARY KEY (role, instance_id)
	);

	-- Readiness reads "the freshest row for this role" and nothing else, so the
	-- index is on the ordering that query uses.
	CREATE INDEX IF NOT EXISTS idx_process_heartbeats_role_seen ON process_heartbeats(role, last_seen_at DESC);
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

// migratePipelines upgrades the Creator aggregate with persisted stage outputs,
// approval evidence, failure evidence and a race-safe create key. The checks
// duplicate domain validation deliberately: direct SQL must not be able to
// create an aggregate the worker cannot safely interpret.
func migratePipelines(db *sql.DB) error {
	migration := `
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS research_reference TEXT;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS script_reference TEXT;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS review_reference TEXT;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS published_reference TEXT;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS failure_reason TEXT;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS retry_count INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS approved_by TEXT;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS approved_at TIMESTAMP;
	ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;
	CREATE INDEX IF NOT EXISTS idx_pipelines_workspace_created ON pipelines(workspace_id, created_at DESC, id DESC);
	CREATE UNIQUE INDEX IF NOT EXISTS uq_pipelines_workspace_idempotency ON pipelines(workspace_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
	DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'pipelines_stage_valid') THEN
			ALTER TABLE pipelines ADD CONSTRAINT pipelines_stage_valid CHECK (stage IN ('research', 'script', 'review', 'publish', 'complete'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'pipelines_status_valid') THEN
			ALTER TABLE pipelines ADD CONSTRAINT pipelines_status_valid CHECK (status IN ('created', 'active', 'awaiting_approval', 'approved', 'done', 'failed'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'pipelines_retry_count_valid') THEN
			ALTER TABLE pipelines ADD CONSTRAINT pipelines_retry_count_valid CHECK (retry_count >= 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'pipelines_stage_status_valid') THEN
			ALTER TABLE pipelines ADD CONSTRAINT pipelines_stage_status_valid CHECK (
				status = 'failed' OR
				(stage = 'research' AND status = 'created') OR
				(stage IN ('script', 'publish') AND status = 'active') OR
				(stage = 'review' AND status IN ('awaiting_approval', 'approved')) OR
				(stage = 'complete' AND status = 'done'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'pipelines_task_fk') THEN
			ALTER TABLE pipelines ADD CONSTRAINT pipelines_task_fk FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL;
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'pipelines_publication_fk') THEN
			ALTER TABLE pipelines ADD CONSTRAINT pipelines_publication_fk FOREIGN KEY (publication_id) REFERENCES publications(id) ON DELETE SET NULL;
		END IF;
	END$$;
	`
	if _, err := db.Exec(migration); err != nil {
		return fmt.Errorf("migrate pipelines: %w", err)
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
	return []string{"workspaces", "departments", "teams", "ai_employees", "ceos", "tasks", "knowledge_documents", "memory_embeddings", "publications", "pipelines", "users", "audit_events",
		"ai_executions", "ai_execution_retries", "retry_dispatch_log", "ai_execution_dlq", "ai_tool_calls", "execution_contexts"}
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

// migrateTasks adds the column constraints and the listing index the task
// HTTP surface depends on.
//
// The CHECK constraints duplicate validation the domain already performs in
// internal/task, and that duplication is deliberate. The service is the place a
// caller is told why a value was refused, but it is not the only path to the
// table: a migration, an operator, or a future writer could all insert a row the
// service never saw. A status the lifecycle does not define would then be
// stored, read back, and fail every transition check with a confusing error
// instead of never having been written.
//
// The lists here must stay identical to the constants in internal/task; a test
// asserts the two agree so they cannot drift apart quietly.
//
// idx_tasks_workspace_created backs the bounded, newest-first listing: the query
// filters on workspace_id (and optionally status) and pages by (created_at, id),
// so without a matching index every page is a full scan of the tenant's tasks
// followed by a sort.
//
// Idempotent, and it runs on every boot, so a database created before the task
// API existed is upgraded rather than left without the constraints.
func migrateTasks(db *sql.DB) error {
	migration := `
	DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_status_valid') THEN
			ALTER TABLE tasks ADD CONSTRAINT tasks_status_valid CHECK (status IN (
				'backlog', 'planned', 'in_progress', 'in_review',
				'completed', 'cancelled', 'rejected', 'failed'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_priority_valid') THEN
			ALTER TABLE tasks ADD CONSTRAINT tasks_priority_valid CHECK (priority IN (
				'low', 'normal', 'high', 'urgent'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_assignee_type_valid') THEN
			ALTER TABLE tasks ADD CONSTRAINT tasks_assignee_type_valid CHECK (assignee_type IN (
				'ai_employee', 'human'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'tasks_title_not_blank') THEN
			ALTER TABLE tasks ADD CONSTRAINT tasks_title_not_blank
				CHECK (length(trim(title)) > 0);
		END IF;
	END$$;

	CREATE INDEX IF NOT EXISTS idx_tasks_workspace_created
		ON tasks(workspace_id, created_at DESC, id DESC);
	`
	if _, err := db.Exec(migration); err != nil {
		// A constraint add can only fail if pre-existing rows already violate
		// the invariant. Surface that loudly: silently skipping the constraint
		// would leave the table accepting values the lifecycle cannot handle.
		return fmt.Errorf("migrate tasks: %w", err)
	}
	return nil
}

// migrateKnowledge pins the invariants the knowledge domain already validates,
// so a row written by any other path cannot carry a kind or a title the domain
// does not recognize. It also adds the index that makes the bounded, newest-first
// listing cheap.
//
// Additive and idempotent: every statement is guarded, so re-running the
// bootstrap neither duplicates a constraint nor rewrites the table. A constraint
// add can only fail if pre-existing rows already violate the invariant, and that
// is surfaced rather than skipped -- silently omitting the constraint would
// leave the table accepting values the domain cannot handle.
func migrateKnowledge(db *sql.DB) error {
	migration := `
	DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_kind_valid') THEN
			ALTER TABLE knowledge_documents ADD CONSTRAINT knowledge_kind_valid CHECK (kind IN (
				'document', 'campaign_rule', 'style_guide'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_title_not_blank') THEN
			ALTER TABLE knowledge_documents ADD CONSTRAINT knowledge_title_not_blank
				CHECK (length(trim(title)) > 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_content_not_blank') THEN
			ALTER TABLE knowledge_documents ADD CONSTRAINT knowledge_content_not_blank
				CHECK (length(trim(content)) > 0);
		END IF;
	END$$;

	CREATE INDEX IF NOT EXISTS idx_knowledge_workspace_created
		ON knowledge_documents(workspace_id, created_at DESC, id DESC);
	`
	if _, err := db.Exec(migration); err != nil {
		return fmt.Errorf("migrate knowledge: %w", err)
	}
	return nil
}

// migrateOrganization adds constraints and indexes for the organizational
// hierarchy: Departments → Teams → AI Employees. It enforces DB-level
// invariants: name not blank, role not blank, unique name within parent
// (case-insensitive), and deterministic listing indexes. It also adds
// updated_at columns to departments and teams for audit consistency.
//
// Idempotent and runs on every boot, so databases created before the org API
// existed are upgraded.
func migrateOrganization(db *sql.DB) error {
	migration := `
	ALTER TABLE departments ADD COLUMN IF NOT EXISTS updated_at TIMESTAMP DEFAULT NOW();
	ALTER TABLE teams ADD COLUMN IF NOT EXISTS updated_at TIMESTAMP DEFAULT NOW();

	DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'departments_name_not_blank') THEN
			ALTER TABLE departments ADD CONSTRAINT departments_name_not_blank CHECK (length(trim(name)) > 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'teams_name_not_blank') THEN
			ALTER TABLE teams ADD CONSTRAINT teams_name_not_blank CHECK (length(trim(name)) > 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_employees_name_not_blank') THEN
			ALTER TABLE ai_employees ADD CONSTRAINT ai_employees_name_not_blank CHECK (length(trim(name)) > 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_employees_role_not_blank') THEN
			ALTER TABLE ai_employees ADD CONSTRAINT ai_employees_role_not_blank CHECK (length(trim(role)) > 0);
		END IF;
	END$$;

	CREATE UNIQUE INDEX IF NOT EXISTS uq_departments_workspace_name ON departments(workspace_id, lower(name));
	CREATE UNIQUE INDEX IF NOT EXISTS uq_teams_department_name ON teams(department_id, lower(name));
	CREATE UNIQUE INDEX IF NOT EXISTS uq_ai_employees_team_name ON ai_employees(team_id, lower(name));

	CREATE INDEX IF NOT EXISTS idx_departments_workspace_created ON departments(workspace_id, created_at DESC, id DESC);
	CREATE INDEX IF NOT EXISTS idx_teams_workspace_created ON teams(department_id, created_at DESC, id DESC);
	CREATE INDEX IF NOT EXISTS idx_ai_employees_workspace_created ON ai_employees(team_id, created_at DESC, id DESC);
	`
	if _, err := db.Exec(migration); err != nil {
		return fmt.Errorf("migrate organization: %w", err)
	}
	return nil
}

// migrateExecutions pins the AI execution lifecycle invariants the domain
// (internal/execution) already validates, so a row written by any other path
// cannot carry a status, attempt count or retry state the runtime cannot
// interpret.
//
// attempt counts STARTED executions, so it can never exceed max_attempts and
// its upper bound is enforced here. A processed retry row must have its lease
// fingerprint cleared (worker_id NULL) so orchestrators never fence against a
// row that is no longer outstanding; the partial unique index below is what
// enforces at most one outstanding retry per execution, which keeps recovery
// and scheduling decisions unambiguous.
//
// It also adds the concurrency cap the admission service reads: without a
// numeric ceiling the capacity check could not be expressed in SQL at all.
//
// Idempotent and runs on every boot, so a database created before execution
// support existed is upgraded rather than left without the constraints.
func migrateExecutions(db *sql.DB) error {
	migration := `
	ALTER TABLE ai_employees ADD COLUMN IF NOT EXISTS max_concurrent_executions INTEGER NOT NULL DEFAULT 3;
	DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_executions_status_valid') THEN
			ALTER TABLE ai_executions ADD CONSTRAINT ai_executions_status_valid CHECK (status IN (
				'pending', 'running', 'completed', 'failed', 'cancelled'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_executions_max_attempts_valid') THEN
			ALTER TABLE ai_executions ADD CONSTRAINT ai_executions_max_attempts_valid CHECK (max_attempts > 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_executions_attempt_nonneg') THEN
			ALTER TABLE ai_executions ADD CONSTRAINT ai_executions_attempt_nonneg CHECK (attempt >= 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_executions_attempt_leq') THEN
			ALTER TABLE ai_executions ADD CONSTRAINT ai_executions_attempt_leq CHECK (attempt <= max_attempts);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_executions_lease_locked') THEN
			ALTER TABLE ai_executions ADD CONSTRAINT ai_executions_lease_locked CHECK (
				(status <> 'running' AND lease_owner IS NULL AND lease_expires_at IS NULL)
				OR
				(status = 'running' AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_employees_max_concurrent_valid') THEN
			ALTER TABLE ai_employees ADD CONSTRAINT ai_employees_max_concurrent_valid
				CHECK (max_concurrent_executions >= 1);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_execution_retries_retry_seq_valid') THEN
			ALTER TABLE ai_execution_retries ADD CONSTRAINT ai_execution_retries_retry_seq_valid CHECK (retry_seq > 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_execution_retries_state_valid') THEN
			ALTER TABLE ai_execution_retries ADD CONSTRAINT ai_execution_retries_state_valid CHECK (state IN (
				'pending', 'claimed', 'dispatched', 'processed'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_execution_retries_outcome_valid') THEN
			ALTER TABLE ai_execution_retries ADD CONSTRAINT ai_execution_retries_outcome_valid
				CHECK (outcome IS NULL OR outcome IN ('success', 'failed', 'cancelled'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_execution_retries_processed_clear') THEN
			ALTER TABLE ai_execution_retries ADD CONSTRAINT ai_execution_retries_processed_clear
				CHECK (state <> 'processed' OR worker_id IS NULL);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'retry_dispatch_log_outcome_valid') THEN
			ALTER TABLE retry_dispatch_log ADD CONSTRAINT retry_dispatch_log_outcome_valid
				CHECK (outcome IN ('success', 'failed'));
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_execution_dlq_error_code_valid') THEN
			ALTER TABLE ai_execution_dlq ADD CONSTRAINT ai_execution_dlq_error_code_valid CHECK (error_code >= 0);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ai_tool_calls_status_valid') THEN
			ALTER TABLE ai_tool_calls ADD CONSTRAINT ai_tool_calls_status_valid CHECK (status IN ('completed', 'failed'));
		END IF;
	END$$;

	CREATE UNIQUE INDEX IF NOT EXISTS uq_ai_execution_retries_one_outstanding
		ON ai_execution_retries(execution_id, workspace_id)
		WHERE state IN ('pending', 'claimed', 'dispatched');
	`
	if _, err := db.Exec(migration); err != nil {
		// A constraint add can only fail if pre-existing rows already violate
		// the invariant. Surface that loudly: silently skipping the constraint
		// would leave the table accepting values the runtime cannot interpret.
		return fmt.Errorf("migrate executions: %w", err)
	}
	return nil
}

// migratePublications adds the delivery outcome and idempotency fields to the
// existing publication aggregate. It is deliberately additive so deployments
// that already have Phase 2 data can roll forward without a destructive reset.
func migratePublications(db *sql.DB) error {
	migration := `
	ALTER TABLE publications ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
	ALTER TABLE publications ADD COLUMN IF NOT EXISTS external_reference TEXT;
	ALTER TABLE publications ADD COLUMN IF NOT EXISTS failure_reason TEXT;
	ALTER TABLE publications ADD COLUMN IF NOT EXISTS published_by TEXT;
	DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'publications_status_valid') THEN
			ALTER TABLE publications ADD CONSTRAINT publications_status_valid CHECK (status IN (
				'queued', 'review', 'approved', 'published', 'failed', 'rejected', 'cancelled'));
		END IF;
	END$$;
	CREATE UNIQUE INDEX IF NOT EXISTS uq_publications_workspace_idempotency
		ON publications(workspace_id, idempotency_key)
		WHERE idempotency_key IS NOT NULL;
	CREATE INDEX IF NOT EXISTS idx_publications_workspace_created
		ON publications(workspace_id, created_at DESC, id DESC);
	`
	if _, err := db.Exec(migration); err != nil {
		return fmt.Errorf("migrate publications: %w", err)
	}
	return nil
}

// enforceWorkspaceIntegrity installs the database-level guarantees that a row's
// workspace cannot disagree with its parent's, and that a workspace name is
// unique.
//
// Both were previously assumed rather than enforced. Every tenant-scoped read
// and write already runs inside a workspace-scoped transaction, so the service
// layer never produced a mismatch; but row level security governs which rows a
// role may *see*, never whether the values inside a row agree with each other.
// A privileged writer, a migration, or a future code path could therefore
// persist an execution pointing at another workspace's task, and the
// authorization layer would faithfully serve a row that should not exist.
//
// The composite foreign keys below make the disagreement impossible rather than
// merely unlikely. Each parent gains UNIQUE (id, workspace_id) to serve as the
// referenced target -- required because a foreign key must point at a unique
// key, and id alone cannot express "this row, in this workspace". Each child
// then references that pair, so a mismatched workspace_id fails the constraint
// on the same insert or update that introduced it.
//
// ai_executions.task_id keeps its original ON DELETE SET NULL behaviour when a
// task is deleted, via the column-scoped form `SET NULL (task_id)`: PostgreSQL 15
// and later can null one column of a composite key, which is required here
// because the other column, workspace_id, is NOT NULL and a plain SET NULL
// would attempt to null it too and fail the delete.
//
// The idempotency-guard style matches the other migrations here: each statement
// is wrapped so a second run is a no-op, which keeps Bootstrap restartable and
// its repeat run free of drift.
func enforceWorkspaceIntegrity(db *sql.DB) error {
	migration := `
	-- Parent keys. UNIQUE (id, workspace_id) is redundant for uniqueness --
	-- id is already the primary key -- and exists solely to give the composite
	-- foreign keys below a valid target.
	DO $$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'uq_tasks_id_workspace' AND conrelid = 'tasks'::regclass
		) THEN
			ALTER TABLE tasks ADD CONSTRAINT uq_tasks_id_workspace
				UNIQUE (id, workspace_id);
		END IF;
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'uq_ai_executions_id_workspace' AND conrelid = 'ai_executions'::regclass
		) THEN
			ALTER TABLE ai_executions ADD CONSTRAINT uq_ai_executions_id_workspace
				UNIQUE (id, workspace_id);
		END IF;
	END$$;

	-- An execution's task must live in the execution's workspace. The
	-- single-column foreign key is dropped rather than kept alongside: two
	-- foreign keys on the same column means two sets of referential triggers
	-- racing to null task_id on a task delete, and the composite key already
	-- subsumes it, still refusing an orphan task_id on its own.
	ALTER TABLE ai_executions DROP CONSTRAINT IF EXISTS ai_executions_task_id_fkey;
	DO $$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'fk_ai_executions_task_workspace' AND conrelid = 'ai_executions'::regclass
		) THEN
			ALTER TABLE ai_executions ADD CONSTRAINT fk_ai_executions_task_workspace
				FOREIGN KEY (task_id, workspace_id) REFERENCES tasks(id, workspace_id)
				ON DELETE SET NULL (task_id);
		END IF;
	END$$;

	-- A retry row's workspace must match the execution it retries. ON DELETE
	-- CASCADE is carried over unchanged and needs no column scoping, because a
	-- deleted execution takes its whole retry history with it.
	DO $$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'fk_ai_execution_retries_execution_workspace'
				AND conrelid = 'ai_execution_retries'::regclass
		) THEN
			ALTER TABLE ai_execution_retries ADD CONSTRAINT fk_ai_execution_retries_execution_workspace
				FOREIGN KEY (execution_id, workspace_id) REFERENCES ai_executions(id, workspace_id)
				ON DELETE CASCADE;
		END IF;
	END$$;

	-- workspaces is the tenant root: it has no workspace_id of its own, so a
	-- name can only be unique across the whole table. The service layer already
	-- returned ErrNameTaken for a taken name; this makes the database the
	-- authority for it, which is the only version of that guarantee that also
	-- holds when two creates are in flight at the same time.
	DO $$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'uq_workspaces_name' AND conrelid = 'workspaces'::regclass
		) THEN
			ALTER TABLE workspaces ADD CONSTRAINT uq_workspaces_name UNIQUE (name);
		END IF;
	END$$;

	-- Superseded by the unique constraint's own index; keeping both would store
	-- the same key twice and let a future writer pick the weaker one.
	DROP INDEX IF EXISTS idx_workspaces_name;
	`
	if _, err := db.Exec(migration); err != nil {
		// Adding these can only fail if existing rows already violate the
		// invariants. Surface it rather than skipping the constraint and
		// leaving the schema quietly weaker than the code assumes.
		return fmt.Errorf("enforce workspace integrity: %w", err)
	}
	return nil
}

func enableRLS(db *sql.DB) error {
	for _, table := range rlsTables() {
		// #nosec G201 -- table is from the fixed rlsTables allowlist.
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
	// #nosec G201 -- adminRole is validated as a restricted SQL identifier above.
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

		-- AI executions: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'ai_executions'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON ai_executions
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- AI execution retries: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'ai_execution_retries'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON ai_execution_retries
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Retry dispatch log: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'retry_dispatch_log'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON retry_dispatch_log
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- AI execution dead-letter queue: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'ai_execution_dlq'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON ai_execution_dlq
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- AI tool calls: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'ai_tool_calls'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON ai_tool_calls
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;

		-- Execution contexts: direct match on workspace_id column
		IF NOT EXISTS (SELECT 1 FROM pg_policy WHERE polname = 'workspace_isolation_policy' AND polrelid = 'execution_contexts'::regclass) THEN
			EXECUTE 'CREATE POLICY workspace_isolation_policy ON execution_contexts
				USING (workspace_id = NULLIF(current_setting(''app.current_workspace'', true), '''')::UUID)';
		END IF;
	END$$;
	`, adminRole, adminRole)
	if _, err := db.Exec(policies); err != nil {
		return fmt.Errorf("create row level security policies: %w", err)
	}
	return nil
}
