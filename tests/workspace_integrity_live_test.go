package austro_os_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// These tests cover the workspace-integrity invariants that Phase C found were
// assumed rather than enforced: a row's workspace_id could disagree with its
// parent's, and a workspace name could be duplicated by two racing writers.
//
// They run against the owner connection deliberately. Row level security
// governs which rows a role may see and never whether the values inside a row
// agree with each other, so proving the constraint requires a connection that
// is not filtered by RLS -- exactly the writer the old single-column foreign
// keys failed to constrain.

// pgErrorCode extracts the SQLSTATE from a driver error, or "" when the error is
// not a PostgreSQL error. Asserting on the code rather than the message is what
// makes these tests state *which* rule refused the write.
func pgErrorCode(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err, "expected the write to be refused")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a PostgreSQL error, got %v", err)
	return pgErr.Code
}

// seedTenant creates the full parent chain a task's owner needs:
// workspace -> department -> team -> ai_employee. Returns the workspace and
// employee ids.
func seedTenant(t *testing.T, db *sql.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ws := uuid.New()
	_, err := db.Exec(`INSERT INTO workspaces (id, name) VALUES ($1, $2)`, ws, "wi-"+ws.String())
	require.NoError(t, err)

	dept := uuid.New()
	_, err = db.Exec(`INSERT INTO departments (id, workspace_id, name) VALUES ($1,$2,$3)`, dept, ws, "dept")
	require.NoError(t, err)

	team := uuid.New()
	_, err = db.Exec(`INSERT INTO teams (id, department_id, name) VALUES ($1,$2,$3)`, team, dept, "team")
	require.NoError(t, err)

	emp := uuid.New()
	_, err = db.Exec(`INSERT INTO ai_employees (id, team_id, name, role) VALUES ($1,$2,$3,$4)`,
		emp, team, "agent", "worker")
	require.NoError(t, err)

	// Reverse dependency order, each with its own key. Cleanup is best effort
	// and logs rather than fails: a leftover row is a finding to report, and
	// raising it here would mask the assertion that actually failed.
	t.Cleanup(func() {
		for _, c := range []struct {
			query string
			id    uuid.UUID
		}{
			{`DELETE FROM ai_employees WHERE id = $1`, emp},
			{`DELETE FROM teams WHERE id = $1`, team},
			{`DELETE FROM departments WHERE id = $1`, dept},
			{`DELETE FROM workspaces WHERE id = $1`, ws},
		} {
			if _, e := db.Exec(c.query, c.id); e != nil {
				t.Logf("cleanup %s (%s): %v", c.query, c.id, e)
			}
		}
	})
	return ws, emp
}

func seedIntegrityTask(t *testing.T, db *sql.DB, ws uuid.UUID, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := db.Exec(`INSERT INTO tasks (id, workspace_id, title, status, priority)
		VALUES ($1,$2,$3,'backlog','normal')`, id, ws, title)
	require.NoError(t, err)
	return id
}

// TestCompositeForeignKeysAreDefined asserts the constraints exist with the
// exact shape the invariants require, rather than trusting that the behavioural
// tests below passed for the right reason.
func TestCompositeForeignKeysAreDefined(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	type want struct {
		table      string
		constraint string
		definition string
	}
	for _, w := range []want{
		{"tasks", "uq_tasks_id_workspace", "UNIQUE (id, workspace_id)"},
		{"ai_executions", "uq_ai_executions_id_workspace", "UNIQUE (id, workspace_id)"},
		{"ai_executions", "fk_ai_executions_task_workspace", "FOREIGN KEY (task_id, workspace_id) REFERENCES tasks(id, workspace_id)"},
		{"ai_execution_retries", "fk_ai_execution_retries_execution_workspace", "FOREIGN KEY (execution_id, workspace_id) REFERENCES ai_executions(id, workspace_id)"},
		{"workspaces", "uq_workspaces_name", "UNIQUE (name)"},
	} {
		t.Run(w.table+"."+w.constraint, func(t *testing.T) {
			var def string
			err := admin.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint
				WHERE conrelid = $1::regclass AND conname = $2`, w.table, w.constraint).Scan(&def)
			require.NoError(t, err, "constraint %s.%s must exist", w.table, w.constraint)
			require.Contains(t, def, w.definition,
				"constraint %s.%s has an unexpected definition", w.table, w.constraint)
		})
	}

	// Every foreign key from ai_executions to tasks must be workspace-scoped. A
	// key that omits workspace_id is exactly the hole this change closes: it lets
	// a row name a parent belonging to a different workspace. Scoped to the
	// tasks edge on purpose -- ai_employee_id references ai_employees(id) and is
	// legitimately single-column.
	var unscoped int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM pg_constraint c JOIN pg_class child ON child.oid = c.conrelid JOIN pg_class parent ON parent.oid = c.confrelid WHERE child.relname = 'ai_executions' AND parent.relname = 'tasks' AND 'workspace_id' <> ALL (SELECT a.attname FROM unnest(c.conkey) AS k(attnum) JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum)`).Scan(&unscoped))
	require.Zero(t, unscoped, "ai_executions still has a tasks foreign key that ignores workspace_id")
}

// TestExecutionTaskWorkspaceMustMatch proves the ai_executions -> tasks invariant
// in both directions: a legitimate insert is accepted and a workspace mismatch
// is refused on insert and on update.
func TestExecutionTaskWorkspaceMustMatch(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	wsA, empA := seedTenant(t, admin)
	wsB, _ := seedTenant(t, admin)
	taskA := seedIntegrityTask(t, admin, wsA, "task in A")

	t.Run("legitimate same-workspace insert succeeds", func(t *testing.T) {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_executions
			(id, workspace_id, ai_employee_id, task_id, request_hash)
			VALUES ($1,$2,$3,$4,'h')`, id, wsA, empA, taskA)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM ai_executions WHERE id = $1`, id) })
	})

	t.Run("mismatched workspace insert is rejected", func(t *testing.T) {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_executions
			(id, workspace_id, ai_employee_id, task_id, request_hash)
			VALUES ($1,$2,$3,$4,'h')`, id, wsB, empA, taskA)
		require.Equal(t, "23503", pgErrorCode(t, err),
			"a foreign key violation is the only acceptable answer")
		require.NoError(t, func() error {
			var n int
			e := admin.QueryRow(`SELECT count(*) FROM ai_executions WHERE id = $1`, id).Scan(&n)
			if e != nil {
				return e
			}
			if n != 0 {
				return fmt.Errorf("rejected insert still left %d row(s)", n)
			}
			return nil
		}())
	})

	t.Run("orphan task insert remains rejected", func(t *testing.T) {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_executions
			(id, workspace_id, ai_employee_id, task_id, request_hash)
			VALUES ($1,$2,$3,$4,'h')`, id, wsA, empA, uuid.New())
		require.Equal(t, "23503", pgErrorCode(t, err))
	})

	t.Run("mismatched workspace update is rejected", func(t *testing.T) {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_executions
			(id, workspace_id, ai_employee_id, task_id, request_hash)
			VALUES ($1,$2,$3,$4,'h')`, id, wsA, empA, taskA)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM ai_executions WHERE id = $1`, id) })

		// Move the execution into another workspace while still pointing at A's
		// task. The composite key must refuse the re-pointing.
		_, err = admin.Exec(`UPDATE ai_executions SET workspace_id = $1 WHERE id = $2`, wsB, id)
		require.Equal(t, "23503", pgErrorCode(t, err))

		var stillA uuid.UUID
		require.NoError(t, admin.QueryRow(`SELECT workspace_id FROM ai_executions WHERE id = $1`, id).Scan(&stillA))
		require.Equal(t, wsA, stillA, "a rejected update must leave the row untouched")
	})
}

// TestRetryExecutionWorkspaceMustMatch proves the ai_execution_retries ->
// ai_executions invariant.
func TestRetryExecutionWorkspaceMustMatch(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	wsA, empA := seedTenant(t, admin)
	wsB, _ := seedTenant(t, admin)
	taskA := seedIntegrityTask(t, admin, wsA, "task in A")

	execID := uuid.New()
	_, err = admin.Exec(`INSERT INTO ai_executions
		(id, workspace_id, ai_employee_id, task_id, request_hash)
		VALUES ($1,$2,$3,$4,'h')`, execID, wsA, empA, taskA)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM ai_executions WHERE id = $1`, execID) })

	t.Run("legitimate same-workspace insert succeeds", func(t *testing.T) {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_execution_retries
			(id, execution_id, workspace_id, retry_seq) VALUES ($1,$2,$3,1)`, id, execID, wsA)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM ai_execution_retries WHERE id = $1`, id) })
	})

	t.Run("mismatched workspace insert is rejected", func(t *testing.T) {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_execution_retries
			(id, execution_id, workspace_id, retry_seq) VALUES ($1,$2,$3,1)`, id, execID, wsB)
		require.Equal(t, "23503", pgErrorCode(t, err))
	})

	t.Run("orphan execution insert remains rejected", func(t *testing.T) {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_execution_retries
			(id, execution_id, workspace_id, retry_seq) VALUES ($1,$2,$3,1)`, id, uuid.New(), wsA)
		require.Equal(t, "23503", pgErrorCode(t, err))
	})

	t.Run("mismatched workspace update is rejected", func(t *testing.T) {
		id := uuid.New()
		_, err := admin.Exec(`INSERT INTO ai_execution_retries
			(id, execution_id, workspace_id, retry_seq) VALUES ($1,$2,$3,2)`, id, execID, wsA)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM ai_execution_retries WHERE id = $1`, id) })

		_, err = admin.Exec(`UPDATE ai_execution_retries SET workspace_id = $1 WHERE id = $2`, wsB, id)
		require.Equal(t, "23503", pgErrorCode(t, err))
	})
}

// TestCompositeForeignKeyDeleteBehaviour proves the existing delete semantics
// survived the change. In particular ON DELETE SET NULL (task_id) must null only
// task_id: workspace_id is NOT NULL, so a plain SET NULL would fail the delete.
func TestCompositeForeignKeyDeleteBehaviour(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	wsA, empA := seedTenant(t, admin)
	taskA := seedIntegrityTask(t, admin, wsA, "task to be deleted")

	execID := uuid.New()
	_, err = admin.Exec(`INSERT INTO ai_executions
		(id, workspace_id, ai_employee_id, task_id, request_hash)
		VALUES ($1,$2,$3,$4,'h')`, execID, wsA, empA, taskA)
	require.NoError(t, err)

	retryID := uuid.New()
	_, err = admin.Exec(`INSERT INTO ai_execution_retries
		(id, execution_id, workspace_id, retry_seq) VALUES ($1,$2,$3,1)`, retryID, execID, wsA)
	require.NoError(t, err)

	t.Run("deleting a task nulls task_id and preserves workspace_id", func(t *testing.T) {
		_, err := admin.Exec(`DELETE FROM tasks WHERE id = $1`, taskA)
		require.NoError(t, err, "deleting a task must still succeed with the composite key in place")

		// task_id is nulled, so it is read as text: scanning a SQL NULL
		// straight into a uuid.UUID is an error rather than a zero value.
		var taskID, wsID sql.NullString
		require.NoError(t, admin.QueryRow(
			`SELECT task_id::text, workspace_id::text FROM ai_executions WHERE id = $1`, execID).Scan(&taskID, &wsID))
		require.False(t, taskID.Valid, "task_id must be set to NULL, not left dangling")
		require.True(t, wsID.Valid, "workspace_id is NOT NULL and must survive")
		require.Equal(t, wsA.String(), wsID.String, "workspace_id must be untouched")
	})

	t.Run("deleting an execution cascades its retries", func(t *testing.T) {
		_, err := admin.Exec(`DELETE FROM ai_executions WHERE id = $1`, execID)
		require.NoError(t, err)
		var n int
		require.NoError(t, admin.QueryRow(
			`SELECT count(*) FROM ai_execution_retries WHERE id = $1`, retryID).Scan(&n))
		require.Zero(t, n, "ON DELETE CASCADE must still remove the retry row")
	})
}

// TestWorkspaceNameUniquenessConstraint proves the database refuses a duplicate
// name rather than the service layer discovering it beforehand.
func TestWorkspaceNameUniquenessConstraint(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	name := "uniq-" + uuid.New().String()
	first := uuid.New()
	_, err = admin.Exec(`INSERT INTO workspaces (id, name) VALUES ($1,$2)`, first, name)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM workspaces WHERE id = $1`, first) })

	_, err = admin.Exec(`INSERT INTO workspaces (id, name) VALUES ($1,$2)`, uuid.New(), name)
	require.Equal(t, "23505", pgErrorCode(t, err),
		"a duplicate workspace name must be a unique violation")

	var n int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM workspaces WHERE name = $1`, name).Scan(&n))
	require.Equal(t, 1, n, "exactly one workspace may carry a given name")
}

// TestWorkspaceNameUniquenessIsRaceSafe is the test the old SELECT ... WHERE NOT
// EXISTS guard could not pass. Every writer is released at the same instant with
// its own transaction, so the reads genuinely overlap; a read-then-write guard
// lets all of them observe the name as free and inserts N copies, while the
// unique constraint admits exactly one.
func TestWorkspaceNameUniquenessIsRaceSafe(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	const writers = 8
	name := "race-" + uuid.New().String()

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := admin.BeginTx(context.Background(), nil)
			if err != nil {
				results[i] = err
				return
			}
			defer tx.Rollback() //nolint:errcheck // rollback-after-commit is expected
			<-start             // release every writer together

			var id uuid.UUID
			err = tx.QueryRow(`INSERT INTO workspaces (id, name) VALUES ($1,$2)
				RETURNING id`, uuid.New(), name).Scan(&id)
			if err != nil {
				results[i] = err
				return
			}
			results[i] = tx.Commit()
		}(i)
	}
	close(start) // the whole point: no writer may start before the others
	wg.Wait()

	var inserted, conflicts int
	for i, err := range results {
		switch {
		case err == nil:
			inserted++
		default:
			var pgErr *pgconn.PgError
			require.True(t, errors.As(err, &pgErr),
				"writer %d failed with a non-PostgreSQL error: %v", i, err)
			require.Equal(t, "23505", pgErr.Code,
				"the only acceptable loser error is a unique violation, got %s", pgErr.Code)
			conflicts++
		}
	}
	require.Equal(t, 1, inserted, "exactly one concurrent writer may create the workspace")
	require.Equal(t, writers-1, conflicts, "every other writer must receive the conflict")

	var n int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM workspaces WHERE name = $1`, name).Scan(&n))
	require.Equal(t, 1, n, "a lost race must not leave an extra row behind")
	t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM workspaces WHERE name = $1`, name) })
}
