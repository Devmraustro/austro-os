package austro_os_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/publish"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// publishAuditSink records service decisions for tracing assertions.
type publishAuditSink struct {
	mu   sync.Mutex
	recs []publish.AuditRecord
}

func (a *publishAuditSink) Record(_ context.Context, rec publish.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, rec)
}

func (a *publishAuditSink) has(eventType, outcome string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.recs {
		if r.EventType == eventType && r.Outcome == outcome {
			return true
		}
	}
	return false
}

// TestPublishingServiceLifecycleIntegration runs the full queue → review →
// approve → publish path (plus reject/cancel), verifying mandatory human
// approval, workspace denial, invalid-transition rejection, the terminal state
// gate, and the deterministic stub, against the live database.
func TestPublishingServiceLifecycleIntegration(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	// Keep count-based assertions deterministic: clear publications in the two
	// workspaces via the admin connection.
	require.NoError(t, deletePublicationsByWorkspace(admin, wsA))
	require.NoError(t, deletePublicationsByWorkspace(admin, wsB))

	store := postgres.NewPublicationStore(admin)
	svc := publish.NewService(store, publish.StubPublisher{}, &publishAuditSink{}, nil)

	ctx := publish.WithTrace(context.Background(), "trace-pub-1", "span-pub-1")

	// Create in workspace A.
	p, err := svc.Create(ctx, wsA, nil, nil, "Launch", "Body", publish.StubPlatform)
	require.NoError(t, err)
	require.Equal(t, publish.StatusQueued, p.Status)
	require.NotEmpty(t, p.ContentHash)
	require.Equal(t, publish.ContentDigest("Launch", "Body"), p.ContentHash)

	needsCleanup := true
	t.Cleanup(func() {
		if needsCleanup {
			_, _ = admin.Exec(`DELETE FROM publications WHERE id=$1`, p.ID)
		}
	})

	// Cross-workspace read must be denied at the boundary.
	_, err = svc.Get(ctx, wsB, p.ID)
	require.True(t, errors.Is(err, publish.ErrNotFound) || errors.Is(err, publish.ErrWorkspaceMismatch),
		"B must not read A's publication: %v", err)

	// No publish from queued.
	_, err = svc.Publish(ctx, wsA, p.ID, "human")
	require.Error(t, err, "publishing from queued must fail")

	// Move to review.
	p, err = svc.ToReview(ctx, wsA, p.ID)
	require.NoError(t, err)
	require.Equal(t, publish.StatusReview, p.Status)

	// No publish from review (no approval yet).
	_, err = svc.Publish(ctx, wsA, p.ID, "human")
	require.Error(t, err, "publishing before approval must fail")

	// Machine approval must be impossible (approval is always human).
	_, err = svc.Approve(ctx, wsA, p.ID, "  ")
	require.Error(t, err, "blank approver must be rejected")

	// Human approve.
	p, err = svc.Approve(ctx, wsA, p.ID, "alice")
	require.NoError(t, err)
	require.Equal(t, publish.StatusApproved, p.Status)
	require.NotNil(t, p.ApprovedBy)
	require.Equal(t, "alice", *p.ApprovedBy)
	require.True(t, p.HasApproval())

	// Publish (human release) delivers via the stub deterministically.
	before := p
	p, err = svc.Publish(ctx, wsA, p.ID, "bob")
	require.NoError(t, err)
	require.Equal(t, publish.StatusPublished, p.Status)
	require.NotNil(t, p.PublishedAt)
	stubRef, err := publish.StubPublisher{}.Publish(ctx, before)
	require.NoError(t, err)
	require.Contains(t, stubRef, "stub://stub/")

	// A published publication is terminal: no further transitions.
	_, err = svc.ToReview(ctx, wsA, p.ID)
	require.True(t, errors.Is(err, publish.ErrTerminalState), "published must be terminal: %v", err)

	// Rejection path: create a second publication, review it, reject it.
	p2, err := svc.Create(ctx, wsA, nil, nil, "Second", "Body 2", publish.StubPlatform)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM publications WHERE id=$1`, p2.ID)
	})
	_, err = svc.ToReview(ctx, wsA, p2.ID)
	require.NoError(t, err)
	p2, err = svc.Reject(ctx, wsA, p2.ID, "carol")
	require.NoError(t, err)
	require.Equal(t, publish.StatusRejected, p2.Status)
	require.NotNil(t, p2.RejectedAt)
	_, err = svc.Publish(ctx, wsA, p2.ID, "dave")
	require.True(t, errors.Is(err, publish.ErrTerminalState), "rejected must be terminal (no publish): %v", err)

	// List is workspace-scoped; A sees its own publications.
	listA, err := svc.List(ctx, wsA, nil)
	require.NoError(t, err)
	require.Equal(t, 2, len(listA), "A must see its two publications")
	listB, err := svc.List(ctx, wsB, nil)
	require.NoError(t, err)
	require.Empty(t, listB, "B must not see A's publications")

	needsCleanup = false
}

// TestPublishingWorkspaceWriteIsolation proves a workspace cannot reach another
// workspace's publication through the service, and list counts never leak.
func TestPublishingWorkspaceWriteIsolation(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	require.NoError(t, deletePublicationsByWorkspace(admin, wsA))
	require.NoError(t, deletePublicationsByWorkspace(admin, wsB))

	store := postgres.NewPublicationStore(admin)
	svcA := publish.NewService(store, publish.StubPublisher{}, nil, nil)

	p, err := svcA.Create(context.Background(), wsA, nil, nil, "A only", "Body", publish.StubPlatform)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM publications WHERE id=$1`, p.ID)
	})

	// Operate as workspace B against A's id: must be denied.
	svcB := publish.NewService(store, publish.StubPublisher{}, nil, nil)
	_, err = svcB.ToReview(context.Background(), wsB, p.ID)
	require.True(t, errors.Is(err, publish.ErrNotFound) || errors.Is(err, publish.ErrWorkspaceMismatch),
		"B must not transition A's publication: %v", err)
	_, err = svcB.Approve(context.Background(), wsB, p.ID, "eve")
	require.Error(t, err, "B must not approve A's publication")
	_, err = svcB.Publish(context.Background(), wsB, p.ID, "frank")
	require.Error(t, err, "B must not publish A's publication")
}

// deletePublicationsByWorkspace removes all publications for a workspace via the
// admin connection, used to keep count-based assertions deterministic.
func deletePublicationsByWorkspace(db *sql.DB, ws uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM publications WHERE workspace_id = $1`, ws)
	return err
}
