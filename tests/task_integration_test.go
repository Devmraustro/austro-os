package austro_os_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/task"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// taskAuditSink records service decisions for tracing assertions.
type taskAuditSink struct {
	mu   sync.Mutex
	recs []task.AuditRecord
}

func (a *taskAuditSink) Record(_ context.Context, rec task.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, rec)
}

func (a *taskAuditSink) has(eventType, outcome string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.recs {
		if r.EventType == eventType && r.Outcome == outcome {
			return true
		}
	}
	return false
}

// TestTasksRLSPolicyEnabled verifies the tasks table carries the standard
// workspace isolation policy and has row-level security enabled (added
// additively by the Phase 2 migration).
func TestTasksRLSPolicyEnabled(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	var n int
	require.NoError(t, admin.QueryRow(
		`SELECT count(*) FROM pg_policy WHERE polname='workspace_isolation_policy' AND polrelid='tasks'::regclass`,
	).Scan(&n))
	require.Equal(t, 1, n, "expected workspace_isolation_policy on tasks")

	var rlsEnabled bool
	require.NoError(t, admin.QueryRow(`SELECT relrowsecurity FROM pg_class WHERE oid='tasks'::regclass`).Scan(&rlsEnabled))
	require.True(t, rlsEnabled, "row-level security must be enabled on tasks")
}

// indicateTaskRLSSeen is a marker so tests that exercise the RLS boundary can
// be recognized; it is intentionally a no-op kept here for clarity.
func indicateTaskRLSSeen() {}

// TestTaskServiceLifecycleIntegration exercises the full task lifecycle through
// the concrete Postgres store against the live database, and verifies that
// cross-workspace access is denied at the service boundary.
func TestTaskServiceLifecycleIntegration(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	store := postgres.NewTaskStore(admin)
	audit := &taskAuditSink{}
	svc := task.NewService(store, audit, task.NullEventSink{})

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	// Seed both workspaces so FKs resolve on insert. ensureIsolationRoles is
	// idempotent and creates the workspace rows if absent.
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	// Isolate this test from prior runs: clear any tasks already present in the
	// two workspaces (admin bypasses RLS, so this is a full cleanup).
	require.NoError(t, deleteTasksByWorkspace(admin, wsA))
	require.NoError(t, deleteTasksByWorkspace(admin, wsB))

	ctx := task.WithTrace(context.Background(), "trace-task", "span-task")

	// Lifecycle for workspace A.
	ta, err := svc.Create(ctx, wsA, "Write the opening scene", task.PriorityUrgent, "first act")
	require.NoError(t, err)
	require.Equal(t, task.StatusBacklog, ta.Status)

	ta, err = svc.Transition(ctx, wsA, ta.ID, task.StatusPlanned)
	require.NoError(t, err)
	require.Equal(t, task.StatusPlanned, ta.Status)

	ta, err = svc.Transition(ctx, wsA, ta.ID, task.StatusInProgress)
	require.NoError(t, err)

	ta, err = svc.Transition(ctx, wsA, ta.ID, task.StatusInReview)
	require.NoError(t, err)

	ta, err = svc.Transition(ctx, wsA, ta.ID, task.StatusCompleted)
	require.NoError(t, err)
	require.Equal(t, task.StatusCompleted, ta.Status)

	require.True(t, audit.has("task.transition", "success"), "expected a task.transition success audit")

	// Terminal state: no further transitions allowed.
	_, err = svc.Transition(ctx, wsA, ta.ID, task.StatusBacklog)
	require.True(t, errors.Is(err, task.ErrTerminalState), "terminal task must reject transitions")

	// Workspace B gets an independent task.
	tb, err := svc.Create(ctx, wsB, "Finalize B script", task.PriorityNormal, "")
	require.NoError(t, err)

	// Cross-workspace read must be denied (no information leakage).
	_, err = svc.Get(ctx, wsB, ta.ID)
	require.True(t, errors.Is(err, task.ErrNotFound) || errors.Is(err, task.ErrWorkspaceMismatch),
		"B must not read A's task: %v", err)

	// Cross-workspace transition must be denied.
	_, err = svc.Transition(ctx, wsB, ta.ID, task.StatusPlanned)
	require.True(t, errors.Is(err, task.ErrNotFound) || errors.Is(err, task.ErrWorkspaceMismatch),
		"B must not transition A's task: %v", err)

	// Workspace-scoped listing.
	listA, err := svc.List(ctx, wsA, nil)
	require.NoError(t, err)
	require.Len(t, listA, 1, "workspace A must expose exactly one task")
	require.Equal(t, ta.ID, listA[0].ID)

	listB, err := svc.List(ctx, wsB, nil)
	require.NoError(t, err)
	require.Len(t, listB, 1, "workspace B must expose exactly one task")
	require.Equal(t, tb.ID, listB[0].ID)

	// List filtered by status stays isolated.
	done := task.StatusCompleted
	listDoneB, err := svc.List(ctx, wsB, &done)
	require.NoError(t, err)
	require.Empty(t, listDoneB, "B has no completed task, so status filter must return none")

	// Delete B's task within its own workspace.
	require.NoError(t, svc.Delete(ctx, wsB, tb.ID))
	_, err = svc.Get(ctx, wsB, tb.ID)
	require.True(t, errors.Is(err, task.ErrNotFound), "deleted task must be gone")
}

// TestTaskServiceUpdateIntegration verifies field updates persist through the
// concrete store and remain workspace-scoped.
func TestTaskServiceUpdateIntegration(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	store := postgres.NewTaskStore(admin)
	svc := task.NewService(store, &taskAuditSink{}, task.NullEventSink{})
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	wsA := uuid.MustParse(workspaceA)
	ctx := context.Background()

	ta, err := svc.Create(ctx, wsA, "Draft title", task.PriorityLow, "description")
	require.NoError(t, err)
	require.NotEmpty(t, ta.Description)

	updated, err := svc.Update(ctx, wsA, ta.ID, "Revised title", task.PriorityHigh,
		taskPtr("new description"), "", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "Revised title", updated.Title)
	require.Equal(t, task.PriorityHigh, updated.Priority)
	require.Equal(t, "new description", updated.Description)

	// Cross-workspace update must be denied.
	_, err = svc.Update(ctx, uuid.MustParse(workspaceB), ta.ID, "hacked", task.PriorityLow, nil, "", nil, nil)
	require.True(t, errors.Is(err, task.ErrNotFound) || errors.Is(err, task.ErrWorkspaceMismatch),
		"cross-workspace update must be denied: %v", err)

	indicateTaskRLSSeen()
}

func taskPtr(s string) *string { return &s }

// deleteTasksByWorkspace removes all tasks for a workspace via the admin
// connection, used to keep count-based integration assertions deterministic.
func deleteTasksByWorkspace(db *sql.DB, workspaceID uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM tasks WHERE workspace_id = $1`, workspaceID)
	return err
}
