package austro_os_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/task"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This file exercises the task store against the real database and the real
// unprivileged runtime role. The handler tests in internal/api run the domain on
// an in-memory store, which is the right place for input validation but cannot
// prove anything about PostgreSQL: only here do row-level security, the CHECK
// constraints and the SQL ordering actually run.

// runtimeDB opens the unprivileged runtime pool the application serves from. It
// is used verbatim rather than through connectAs, which rewrites the user: the
// whole point is to be the role production uses.
func taskRuntimeDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := envOrDefault("AUSTRO_POSTGRES_RUNTIME_DSN",
		"postgres://austro_app:austro-runtime-pw@postgres:5432/austro?sslmode=disable")
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx),
		"the runtime role must be able to connect")
	return db
}

// cleanupWithAdmin removes test rows after the test has finished.
//
// It opens its own connection rather than reusing the one the test deferred a
// Close() on: t.Cleanup callbacks run after the test function's defers, so a
// cleanup holding the test's handle would execute against a closed pool and
// silently leave every row behind. The leak is invisible in a passing run and
// only shows up as a slowly growing database.
func cleanupWithAdmin(t *testing.T, stmt string, args ...any) {
	t.Helper()
	t.Cleanup(func() {
		admin, err := getEnv().AdminDB()
		if err != nil {
			t.Logf("cleanup could not open an admin connection: %v", err)
			return
		}
		defer admin.Close()
		if _, err := admin.Exec(stmt, args...); err != nil {
			t.Logf("cleanup failed for %q: %v", stmt, err)
		}
	})
}

// seedTask inserts a task through the admin connection and removes it afterwards.
func seedTask(t *testing.T, admin *sql.DB, ws uuid.UUID, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := admin.Exec(`INSERT INTO tasks (id, workspace_id, title, status, priority)
		VALUES ($1,$2,$3,'backlog','normal')`, id, ws, title)
	require.NoError(t, err)
	cleanupWithAdmin(t, `DELETE FROM tasks WHERE id = $1`, id)
	return id
}

// TestTaskDeleteIsBlockedAcrossWorkspaces closes the one gap the existing RLS
// test left: it proved A cannot read or update B's task, but not that A cannot
// delete it. Deletion is the most destructive of the three and the one whose
// failure is hardest to notice, because a successful cross-tenant delete leaves
// no row behind to inspect.
func TestTaskDeleteIsBlockedAcrossWorkspaces(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	grantTasksToRestrictedRoles(t, admin, workspaceARole, workspaceBRole)

	wsB := uuid.MustParse(workspaceB)
	taskB := seedTask(t, admin, wsB, "belongs to B")

	dbA := connectRestricted(t, workspaceARole)
	defer dbA.Close()
	setWorkspace(t, dbA, workspaceA)

	res, err := dbA.Exec(`DELETE FROM tasks WHERE id = $1`, taskB)
	require.NoError(t, err, "the statement is permitted; row-level security is what must refuse it")
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Zero(t, affected, "workspace A must not delete workspace B's task")

	// And the row is genuinely still there.
	var still int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM tasks WHERE id = $1`, taskB).Scan(&still))
	require.Equal(t, 1, still, "the task must survive the cross-workspace delete attempt")

	// A can delete its own.
	wsA := uuid.MustParse(workspaceA)
	taskA := seedTask(t, admin, wsA, "belongs to A")
	res, err = dbA.Exec(`DELETE FROM tasks WHERE id = $1`, taskA)
	require.NoError(t, err)
	affected, err = res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "workspace A must be able to delete its own task")
}

// TestTaskConstraintsRejectInvalidValues proves the CHECK constraints added by
// migrateTasks are live. The service validates the same values, but the service
// is not the only path to the table: a migration or an operator could otherwise
// write a status the lifecycle does not define, which would then fail every
// later transition check with a confusing error.
func TestTaskConstraintsRejectInvalidValues(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	wsA := uuid.MustParse(workspaceA)

	cases := map[string]string{
		"unknown status": `INSERT INTO tasks (workspace_id, title, status, priority)
			VALUES ($1, 'x', 'archived', 'normal')`,
		"unknown priority": `INSERT INTO tasks (workspace_id, title, status, priority)
			VALUES ($1, 'x', 'backlog', 'whenever')`,
		"unknown assignee type": `INSERT INTO tasks (workspace_id, title, status, priority, assignee_type)
			VALUES ($1, 'x', 'backlog', 'normal', 'robot')`,
		"blank title": `INSERT INTO tasks (workspace_id, title, status, priority)
			VALUES ($1, '   ', 'backlog', 'normal')`,
		"empty title": `INSERT INTO tasks (workspace_id, title, status, priority)
			VALUES ($1, '', 'backlog', 'normal')`,
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := admin.Exec(stmt, wsA)
			require.Error(t, err,
				"the database must reject %s even though no service validated it", name)
		})
	}

	// Every value the domain accepts must be accepted by the table, so the two
	// lists cannot drift apart in the other direction.
	for _, status := range []string{"backlog", "planned", "in_progress", "in_review",
		"completed", "cancelled", "rejected", "failed"} {
		t.Run("accepts status "+status, func(t *testing.T) {
			id := uuid.New()
			_, err := admin.Exec(`INSERT INTO tasks (id, workspace_id, title, status, priority)
				VALUES ($1, $2, 'x', $3, 'normal')`, id, wsA, status)
			require.NoError(t, err, "%s is a valid lifecycle stage", status)
			cleanupWithAdmin(t, `DELETE FROM tasks WHERE id = $1`, id)
		})
	}
}

// TestTaskStoreListPageOnPostgres checks the bounded listing against the real
// query: the cap, the ordering, and a keyset cursor that covers every row
// exactly once. The in-memory fake cannot catch a wrong row comparison or a
// missing ORDER BY.
func TestTaskStoreListPageOnPostgres(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	// A dedicated workspace rather than the shared workspaceA fixture: that
	// workspace accumulates rows across the suite, so an absolute count over it
	// fails on a warm database even when the pagination is correct. tasks has a
	// foreign key on workspace_id, so the tenant has to be a real row.
	wsA := uuid.New()
	_, err = admin.Exec(`INSERT INTO workspaces (id, name) VALUES ($1, $2)`,
		wsA, "task pagination tenant")
	require.NoError(t, err)
	// Tasks first, then the workspace they reference.
	// Cleanups run last-registered-first, so the workspace is dropped before
	// its tasks are deleted. Register the workspace deletion first.
	cleanupWithAdmin(t, `DELETE FROM workspaces WHERE id = $1`, wsA)
	cleanupWithAdmin(t, `DELETE FROM tasks WHERE workspace_id = $1`, wsA)
	wsB := uuid.MustParse(workspaceB)
	store := postgres.NewTaskStore(taskRuntimeDB(t))

	const total = 9
	base := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	var ids []uuid.UUID
	for i := 0; i < total; i++ {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO tasks (id, workspace_id, title, status, priority, created_at, updated_at)
			VALUES ($1,$2,$3,'backlog','normal',$4,$4)`,
			id, wsA, "task", base.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
		cleanupWithAdmin(t, `DELETE FROM tasks WHERE id = $1`, id)
		ids = append(ids, id)
	}
	// A task in another workspace must never surface.
	other := seedTask(t, admin, wsB, "elsewhere")
	_ = other

	ctx := context.Background()

	t.Run("page is capped", func(t *testing.T) {
		page, err := store.ListPage(ctx, wsA, task.ListQuery{Limit: 4})
		require.NoError(t, err)
		require.Len(t, page.Tasks, 4, "the page must not exceed the requested limit")
		require.Equal(t, 4, page.Limit)
		require.NotEmpty(t, page.NextCursor, "more rows exist, so a cursor must be offered")
	})

	t.Run("oversized limit is clamped", func(t *testing.T) {
		page, err := store.ListPage(ctx, wsA, task.ListQuery{Limit: 100000})
		require.NoError(t, err)
		require.Equal(t, task.MaxPageSize, page.Limit)
		require.Empty(t, page.NextCursor, "fewer rows than the page size means no further page")
	})

	t.Run("ordering is newest first and total", func(t *testing.T) {
		page, err := store.ListPage(ctx, wsA, task.ListQuery{Limit: task.MaxPageSize})
		require.NoError(t, err)
		for i := 1; i < len(page.Tasks); i++ {
			prev, cur := page.Tasks[i-1], page.Tasks[i]
			require.True(t,
				prev.CreatedAt.After(cur.CreatedAt) ||
					(prev.CreatedAt.Equal(cur.CreatedAt) && prev.ID.String() > cur.ID.String()),
				"tasks must be ordered by (created_at, id) descending")
		}
	})

	t.Run("cursor covers every task exactly once", func(t *testing.T) {
		seen := map[uuid.UUID]bool{}
		cursor := ""
		for i := 0; i < 50; i++ {
			q := task.ListQuery{Limit: 2}
			if cursor != "" {
				c, err := task.DecodeCursor(cursor)
				require.NoError(t, err)
				q.Before = c
			}
			page, err := store.ListPage(ctx, wsA, q)
			require.NoError(t, err)
			if len(page.Tasks) == 0 {
				break
			}
			for _, tk := range page.Tasks {
				require.False(t, seen[tk.ID], "task %s returned twice: the cursor repeats rows", tk.ID)
				require.Equal(t, wsA, tk.WorkspaceID,
					"another workspace's task leaked into the page")
				seen[tk.ID] = true
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		for _, id := range ids {
			require.True(t, seen[id], "task %s was never returned: the cursor skipped a row", id)
		}
		require.Len(t, seen, total)
	})
}

// TestTaskStoreIsConfinedByRLS drives the production store over the runtime pool
// and proves the confinement is PostgreSQL's, not a WHERE clause in Go: an
// operation naming another tenant's task affects nothing.
func TestTaskStoreIsConfinedByRLS(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	store := postgres.NewTaskStore(taskRuntimeDB(t))
	ctx := context.Background()

	taskB := seedTask(t, admin, wsB, "belongs to B")

	t.Run("get is not found across the boundary", func(t *testing.T) {
		_, err := store.Get(ctx, wsA, taskB)
		require.ErrorIs(t, err, task.ErrNotFound,
			"workspace A must not read workspace B's task")
	})

	t.Run("update affects nothing across the boundary", func(t *testing.T) {
		stolen := &task.Task{ID: taskB, WorkspaceID: wsA, Title: "stolen",
			Status: task.StatusBacklog, Priority: task.PriorityNormal,
			AssigneeType: task.AssigneeAI}
		_, err := store.Update(ctx, wsA, stolen)
		require.Error(t, err, "workspace A must not update workspace B's task")

		var title string
		require.NoError(t, admin.QueryRow(`SELECT title FROM tasks WHERE id = $1`, taskB).Scan(&title))
		require.Equal(t, "belongs to B", title, "the row must be untouched")
	})

	t.Run("delete affects nothing across the boundary", func(t *testing.T) {
		err := store.Delete(ctx, wsA, taskB)
		require.Error(t, err, "workspace A must not delete workspace B's task")

		var still int
		require.NoError(t, admin.QueryRow(`SELECT count(*) FROM tasks WHERE id = $1`, taskB).Scan(&still))
		require.Equal(t, 1, still)
	})

	t.Run("listing is confined", func(t *testing.T) {
		page, err := store.ListPage(ctx, wsA, task.ListQuery{Limit: task.MaxPageSize})
		require.NoError(t, err)
		for _, tk := range page.Tasks {
			require.Equal(t, wsA, tk.WorkspaceID,
				"the listing leaked a task owned by %s", tk.WorkspaceID)
		}
	})

	t.Run("a workspace with no rows yields an empty page", func(t *testing.T) {
		empty := uuid.New()
		page, err := store.ListPage(ctx, empty, task.ListQuery{Limit: 10})
		require.NoError(t, err)
		require.Empty(t, page.Tasks)
		require.Empty(t, page.NextCursor)
	})
}

// TestTaskStoreCreateRunsAsTheRuntimeRole proves the write path works under the
// unprivileged role and lands in the right tenant -- the complement to the
// isolation assertions above, which would also pass if inserts simply failed.
func TestTaskStoreCreateRunsAsTheRuntimeRole(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	wsA := uuid.MustParse(workspaceA)
	store := postgres.NewTaskStore(taskRuntimeDB(t))
	ctx := context.Background()

	created, err := store.Create(ctx, &task.Task{
		WorkspaceID:  wsA,
		Title:        "created by the runtime role",
		Description:  "body",
		Status:       task.StatusBacklog,
		Priority:     task.PriorityHigh,
		AssigneeType: task.AssigneeAI,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, created.ID)
	cleanupWithAdmin(t, `DELETE FROM tasks WHERE id = $1`, created.ID)

	// Read it back through the same store and through the admin handle.
	got, err := store.Get(ctx, wsA, created.ID)
	require.NoError(t, err)
	require.Equal(t, "created by the runtime role", got.Title)
	require.Equal(t, task.PriorityHigh, got.Priority)

	var ws uuid.UUID
	require.NoError(t, admin.QueryRow(`SELECT workspace_id FROM tasks WHERE id = $1`, created.ID).Scan(&ws))
	require.Equal(t, wsA, ws, "the task must be owned by the workspace it was created in")
}

// TestTaskPaginationIndexExists checks the index that makes the bounded listing
// cheap. Without it every page is a full scan of the tenant's tasks plus a sort,
// which is a performance cliff rather than a correctness bug -- and therefore
// easy to lose silently.
func TestTaskPaginationIndexExists(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	var found bool
	require.NoError(t, admin.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM pg_class c
			JOIN pg_index i ON i.indexrelid = c.oid
			WHERE i.indrelid = 'tasks'::regclass
			  AND c.relname = 'idx_tasks_workspace_created')`).Scan(&found))
	require.True(t, found,
		"idx_tasks_workspace_created must exist to back the bounded, newest-first listing")
}

// TestTaskRuntimeRoleHasNoPrivilegedEscape asserts the properties that make the
// above isolation meaningful, specifically for the tasks table: the runtime role
// is not a superuser, has no BYPASSRLS, does not own the table, and row-level
// security is forced on it.
func TestTaskRuntimeRoleHasNoPrivilegedEscape(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	var superuser, bypass, owns bool
	require.NoError(t, admin.QueryRow(`
		SELECT r.rolsuper, r.rolbypassrls,
		       EXISTS(SELECT 1 FROM pg_class c
		              JOIN pg_roles o ON o.oid = c.relowner
		              WHERE c.relname = 'tasks' AND o.rolname = r.rolname)
		FROM pg_roles r WHERE r.rolname = 'austro_app'`).
		Scan(&superuser, &bypass, &owns))
	require.False(t, superuser, "the runtime role must not be a superuser")
	require.False(t, bypass, "the runtime role must not have BYPASSRLS")
	require.False(t, owns, "the runtime role must not own tasks")

	var rls, forced bool
	require.NoError(t, admin.QueryRow(`
		SELECT relrowsecurity, relforcerowsecurity
		FROM pg_class WHERE relname = 'tasks'`).Scan(&rls, &forced))
	require.True(t, rls, "row-level security must be enabled on tasks")
	require.True(t, forced,
		"row-level security must be forced on tasks, so it applies to the table owner too")

	var policies int
	require.NoError(t, admin.QueryRow(`
		SELECT count(*) FROM pg_policy WHERE polrelid = 'tasks'::regclass`).Scan(&policies))
	require.Equal(t, 1, policies,
		"tasks must have exactly the one workspace isolation policy")
}
