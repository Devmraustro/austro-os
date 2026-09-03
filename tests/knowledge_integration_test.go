package austro_os_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/ai"
	"austro-os/internal/knowledge"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// knowledgeAuditSink records service decisions for tracing assertions.
type knowledgeAuditSink struct {
	mu   sync.Mutex
	recs []knowledge.AuditRecord
}

func (a *knowledgeAuditSink) Record(_ context.Context, rec knowledge.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, rec)
}

func (a *knowledgeAuditSink) has(eventType, outcome string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.recs {
		if r.EventType == eventType && r.Outcome == outcome {
			return true
		}
	}
	return false
}

// TestKnowledgeServiceLifecycleIntegration exercises create/embed/list/get/
// search/delete through the concrete Postgres store and the AI gateway stub
// against the live database, verifying cross-workspace denial at the boundary.
func TestKnowledgeServiceLifecycleIntegration(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	// Keep count-based assertions deterministic: clear knowledge rows in the
	// two workspaces via the admin connection.
	require.NoError(t, deleteKnowledgeByWorkspace(admin, wsA))
	require.NoError(t, deleteKnowledgeByWorkspace(admin, wsB))

	store := postgres.NewKnowledgeStore(admin)
	embedder := knowledge.NewGatewayEmbedder(ai.StubProvider{})
	audit := &knowledgeAuditSink{}
	svc := knowledge.NewService(store, embedder, audit, nil, knowledge.DefaultEmbeddingDimensions)

	ctx := knowledge.WithTrace(context.Background(), "trace-kn-1", "span-kn-1")

	// Create a document in workspace A.
	da, err := svc.Create(ctx, wsA, knowledge.KindDocument, "Company Knowledge", "We ship fast.")
	require.NoError(t, err)
	require.Equal(t, wsA, da.WorkspaceID)
	require.Len(t, da.Embedding, knowledge.DefaultEmbeddingDimensions)
	require.True(t, audit.has("knowledge.create", "success"), "expected a knowledge.create success audit")

	// Get within workspace A.
	got, err := svc.Get(ctx, wsA, da.ID)
	require.NoError(t, err)
	require.Equal(t, "Company Knowledge", got.Title)

	// Cross-workspace read must be denied.
	_, err = svc.Get(ctx, wsB, da.ID)
	require.True(t, errors.Is(err, knowledge.ErrNotFound) || errors.Is(err, knowledge.ErrWorkspaceMismatch),
		"B must not read A's document: %v", err)

	// List is workspace-scoped and kind-filterable.
	listA, err := svc.List(ctx, wsA, nil)
	require.NoError(t, err)
	require.Len(t, listA, 1, "workspace A must expose exactly one document")

	kindDoc := knowledge.KindDocument
	listDoc, err := svc.List(ctx, wsA, &kindDoc)
	require.NoError(t, err)
	require.Len(t, listDoc, 1, "kind filter must match the created document")

	kindRule := knowledge.KindCampaignRule
	listRule, err := svc.List(ctx, wsA, &kindRule)
	require.NoError(t, err)
	require.Empty(t, listRule, "no campaign rule exists, kind filter must return none")

	// Search returns the created document (deterministic stub embedding).
	res, err := svc.Search(ctx, wsA, "knowledge", nil, 5)
	require.NoError(t, err)
	require.NotEmpty(t, res, "search must return at least the A document")
	require.True(t, audit.has("knowledge.search", "success"), "expected a knowledge.search success audit")

	// Delete within workspace A, then confirm gone.
	require.NoError(t, svc.Delete(ctx, wsA, da.ID))
	_, err = svc.Get(ctx, wsA, da.ID)
	require.True(t, errors.Is(err, knowledge.ErrNotFound), "deleted document must be gone")
}

// deleteKnowledgeByWorkspace removes all knowledge rows for a workspace via the
// admin connection, used to keep count-based assertions deterministic.
func deleteKnowledgeByWorkspace(db *sql.DB, ws uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM knowledge_documents WHERE workspace_id = $1`, ws)
	return err
}
