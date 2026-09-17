package austro_os_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/orchestration"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// pipelineAuditSink records service decisions for tracing assertions.
type pipelineAuditSink struct {
	mu   sync.Mutex
	recs []orchestration.AuditRecord
}

func (a *pipelineAuditSink) Record(_ context.Context, rec orchestration.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, rec)
}

func (a *pipelineAuditSink) has(eventType, outcome string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.recs {
		if r.EventType == eventType && r.Outcome == outcome {
			return true
		}
	}
	return false
}

// TestCreatorPipelineLifecycleIntegration runs the full research → script →
// review → publish → complete pipeline through the concrete Postgres store and
// deterministic stubs against the live database, including cross-workspace
// denial and the event-driven handler composition.
func TestCreatorPipelineLifecycleIntegration(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	require.NoError(t, deletePipelinesByWorkspace(admin, wsA))
	require.NoError(t, deletePipelinesByWorkspace(admin, wsB))

	store := postgres.NewPipelineStore(admin)
	audit := &pipelineAuditSink{}
	svc := orchestration.NewService(store,
		orchestration.StubResearcher{}, orchestration.StubScriptWriter{},
		orchestration.StubReviewer{}, orchestration.StubPublisher{}, audit, nil)

	p, err := svc.Create(context.Background(), wsA, nil, "trace-pipe-1")
	require.NoError(t, err)
	require.Equal(t, orchestration.StageResearch, p.Stage)
	require.Equal(t, orchestration.StatusCreated, p.Status)
	require.True(t, audit.has("pipeline.create", "success"), "expected a pipeline.create success audit")

	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM pipelines WHERE id=$1`, p.ID)
	})

	// Cross-workspace read must be denied at the boundary.
	_, err = svc.Get(context.Background(), wsB, p.ID)
	require.True(t, errors.Is(err, orchestration.ErrNotFound) || errors.Is(err, orchestration.ErrWorkspaceMismatch),
		"B must not read A's pipeline: %v", err)

	// Advance through the full loop via the event-driven handler.
	handler := orchestration.NewHandler(svc)
	next := orchestration.StageResearch
	for _, want := range []orchestration.Stage{
		orchestration.StageScript,
		orchestration.StageReview,
		orchestration.StagePublish,
		orchestration.StageComplete,
	} {
		if want == orchestration.StagePublish {
			_, err := svc.Approve(context.Background(), wsA, p.ID, "integration-admin")
			require.NoError(t, err, "approve reviewed pipeline")
		}
		up, err := handler.AdvanceFromEvent(context.Background(), wsA, p.ID, next, "trace-pipe-1", "span-1")
		require.NoError(t, err, "advance to %s", want)
		require.Equal(t, want, up.Stage, "expected stage %s", want)
		next = want
	}

	final, err := svc.Get(context.Background(), wsA, p.ID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StageComplete, final.Stage)
	require.Equal(t, orchestration.StatusDone, final.Status)
	require.True(t, audit.has("pipeline.advance", "success"), "expected pipeline.advance success audit")

	// terminal: no transitions out of done.
	_, err = handler.AdvanceFromEvent(context.Background(), wsA, p.ID, orchestration.StageComplete, "trace-pipe-1", "span-2")
	require.True(t, errors.Is(err, orchestration.ErrTerminalState), "done must be terminal: %v", err)

	// List is workspace-scoped.
	listA, err := svc.List(context.Background(), wsA)
	require.NoError(t, err)
	require.Len(t, listA, 1, "A must see its one pipeline")
	listB, err := svc.List(context.Background(), wsB)
	require.NoError(t, err)
	require.Empty(t, listB, "B must not see A's pipeline")
}

// TestCreatorPipelineWorkspaceWriteIsolation proves a workspace cannot reach
// another workspace's pipeline through the service.
func TestCreatorPipelineWorkspaceWriteIsolation(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	require.NoError(t, deletePipelinesByWorkspace(admin, wsA))
	require.NoError(t, deletePipelinesByWorkspace(admin, wsB))

	store := postgres.NewPipelineStore(admin)
	svcA := orchestration.NewService(store, nil, nil, nil, nil, nil, nil)
	p, err := svcA.Create(context.Background(), wsA, nil, "trace-ws")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM pipelines WHERE id=$1`, p.ID)
	})

	svcB := orchestration.NewService(store, nil, nil, nil, nil, nil, nil)
	_, err = svcB.Advance(context.Background(), wsB, p.ID, orchestration.StageResearch, orchestration.StageScript)
	require.True(t, errors.Is(err, orchestration.ErrNotFound) || errors.Is(err, orchestration.ErrWorkspaceMismatch),
		"B must not advance A's pipeline: %v", err)
}

// deletePipelinesByWorkspace removes all pipelines for a workspace via the
// admin connection, used to keep count-based assertions deterministic.
func deletePipelinesByWorkspace(db *sql.DB, ws uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM pipelines WHERE workspace_id = $1`, ws)
	return err
}
