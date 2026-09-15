package austro_os_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/ai"
	"austro-os/internal/knowledge"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This file exercises the knowledge store against the real database and the real
// unprivileged runtime role. The handler tests in internal/api run the domain on
// an in-memory store, which cannot prove anything about PostgreSQL: only here do
// row-level security, the CHECK constraints, the cosine ranking and the SQL
// ordering actually run.
//
// Tests here create their own workspace rather than reusing the workspaceA
// fixture. TestKnowledgeServiceLifecycleIntegration deletes every knowledge row
// in workspaces A and B as part of its setup, so a count or a "must still exist"
// assertion over that shared tenant would pass or fail depending on which test
// ran first.

// knowledgeTenant creates a dedicated workspace for one test and removes it,
// with its documents, afterwards.
func knowledgeTenant(t *testing.T, admin *sql.DB, name string) uuid.UUID {
	t.Helper()
	ws := uuid.New()
	_, err := admin.Exec(`INSERT INTO workspaces (id, name) VALUES ($1, $2)`, ws, name)
	require.NoError(t, err)
	// Cleanups run last-registered-first, so register the workspace deletion
	// first and its documents second: the documents must go before the row they
	// reference.
	cleanupWithAdmin(t, `DELETE FROM workspaces WHERE id = $1`, ws)
	cleanupWithAdmin(t, `DELETE FROM knowledge_documents WHERE workspace_id = $1`, ws)
	return ws
}

// seedKnowledge inserts a document through the admin connection. The embedding is
// a real 10-dimensional vector so the cosine operator has something to rank.
func seedKnowledge(t *testing.T, admin *sql.DB, ws uuid.UUID, kind, title string, at time.Time, seed float64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	vec := "["
	for i := 0; i < knowledge.DefaultEmbeddingDimensions; i++ {
		if i > 0 {
			vec += ","
		}
		// %g with -1 precision matches the store's own vectorLiteral encoding, so
		// a vector written by this helper has the same form as one the
		// application writes.
		vec += strconv.FormatFloat(float64(float32(seed)+float32(i)*0.01), 'g', -1, 32)
	}
	vec += "]"
	// The id is supplied explicitly rather than left to the column's
	// gen_random_uuid() default. Returning a locally generated UUID while the
	// table generated its own would hand back an id that matches no row, and
	// every later lookup would fail for a reason that has nothing to do with
	// what the test is checking.
	_, err := admin.Exec(`
		INSERT INTO knowledge_documents
			(id, workspace_id, kind, title, content, embedding, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6::vector,$7,$7)`,
		id, ws, kind, title, "content for "+title, vec, at)
	require.NoError(t, err)
	// Prove the row is really there before any assertion depends on it. Without
	// this, a test that expects a lookup to fail cannot tell "blocked by
	// isolation" apart from "was never created".
	var n int
	require.NoError(t, admin.QueryRow(
		`SELECT count(*) FROM knowledge_documents WHERE id = $1`, id).Scan(&n))
	require.Equal(t, 1, n, "the seeded document must exist before the test relies on it")
	cleanupWithAdmin(t, `DELETE FROM knowledge_documents WHERE id = $1`, id)
	return id
}

// seedKnowledgeVec inserts a document with an explicit embedding, for tests that
// depend on the direction of the vector rather than just having one.
func seedKnowledgeVec(t *testing.T, admin *sql.DB, ws uuid.UUID, title string, at time.Time, vec []float32) uuid.UUID {
	t.Helper()
	lit := "["
	for i, v := range vec {
		if i > 0 {
			lit += ","
		}
		lit += strconv.FormatFloat(float64(v), 'g', -1, 32)
	}
	lit += "]"
	id := uuid.New()
	_, err := admin.Exec(`
		INSERT INTO knowledge_documents
			(id, workspace_id, kind, title, content, embedding, created_at, updated_at)
		VALUES ($1,$2,'document',$3,$4,$5::vector,$6,$6)`,
		id, ws, title, "content", lit, at)
	require.NoError(t, err)
	cleanupWithAdmin(t, `DELETE FROM knowledge_documents WHERE id = $1`, id)
	return id
}

// TestKnowledgeConstraintsRejectInvalidValues proves the CHECK constraints added
// by migrateKnowledge are live. The service validates the same values, but the
// service is not the only path to the table, and a row carrying a kind the domain
// does not recognize would later fail every read-side classification with an
// error that points nowhere useful.
func TestKnowledgeConstraintsRejectInvalidValues(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	ws := knowledgeTenant(t, admin, "knowledge constraints")

	cases := map[string]string{
		"unknown kind": `INSERT INTO knowledge_documents (workspace_id, kind, title, content)
			VALUES ($1, 'memo', 'x', 'body')`,
		"blank title": `INSERT INTO knowledge_documents (workspace_id, kind, title, content)
			VALUES ($1, 'document', '   ', 'body')`,
		"empty title": `INSERT INTO knowledge_documents (workspace_id, kind, title, content)
			VALUES ($1, 'document', '', 'body')`,
		"blank content": `INSERT INTO knowledge_documents (workspace_id, kind, title, content)
			VALUES ($1, 'document', 'x', '   ')`,
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := admin.Exec(stmt, ws)
			require.Error(t, err,
				"the database must reject %s even though no service validated it", name)
		})
	}

	for _, kind := range []string{"document", "campaign_rule", "style_guide"} {
		t.Run("accepts kind "+kind, func(t *testing.T) {
			id := uuid.New()
			_, err := admin.Exec(`
				INSERT INTO knowledge_documents (id, workspace_id, kind, title, content)
				VALUES ($1, $2, $3, 'x', 'body')`, id, ws, kind)
			require.NoError(t, err, "%s is a recognized kind", kind)
			cleanupWithAdmin(t, `DELETE FROM knowledge_documents WHERE id = $1`, id)
		})
	}
}

// TestKnowledgeStoreListPageOnPostgres checks the bounded listing against the
// real query. The previous List had neither LIMIT nor ORDER BY, so this is the
// test that proves the replacement is both bounded and deterministic.
func TestKnowledgeStoreListPageOnPostgres(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	ws := knowledgeTenant(t, admin, "knowledge listing")
	other := knowledgeTenant(t, admin, "knowledge listing other")
	store := postgres.NewKnowledgeStore(taskRuntimeDB(t))

	const total = 9
	base := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	var ids []uuid.UUID
	for i := 0; i < total; i++ {
		ids = append(ids, seedKnowledge(t, admin, ws, "document", "doc",
			base.Add(time.Duration(i)*time.Minute), 0.1))
	}
	// A document in another tenant must never surface in this one's listing.
	foreign := seedKnowledge(t, admin, other, "document", "elsewhere", base, 0.1)
	_ = foreign

	ctx := context.Background()

	t.Run("page is capped", func(t *testing.T) {
		page, err := store.ListPage(ctx, ws, knowledge.ListQuery{Limit: 4})
		require.NoError(t, err)
		require.Len(t, page.Documents, 4, "the page must not exceed the requested limit")
		require.Equal(t, 4, page.Limit)
		require.NotEmpty(t, page.NextCursor, "more rows exist, so a cursor must be offered")
	})

	t.Run("oversized limit is clamped", func(t *testing.T) {
		page, err := store.ListPage(ctx, ws, knowledge.ListQuery{Limit: 100000})
		require.NoError(t, err)
		require.Equal(t, knowledge.MaxPageSize, page.Limit)
		require.Empty(t, page.NextCursor, "fewer rows than the page size means no further page")
	})

	t.Run("ordering is newest first and total", func(t *testing.T) {
		page, err := store.ListPage(ctx, ws, knowledge.ListQuery{Limit: knowledge.MaxPageSize})
		require.NoError(t, err)
		require.Len(t, page.Documents, total)
		for i := 1; i < len(page.Documents); i++ {
			prev, cur := page.Documents[i-1], page.Documents[i]
			require.True(t,
				prev.CreatedAt.After(cur.CreatedAt) ||
					(prev.CreatedAt.Equal(cur.CreatedAt) && prev.ID.String() > cur.ID.String()),
				"documents must be ordered by (created_at, id) descending")
		}
	})

	t.Run("cursor covers every document exactly once", func(t *testing.T) {
		seen := map[uuid.UUID]int{}
		cursor := ""
		for i := 0; i < 100; i++ {
			q := knowledge.ListQuery{Limit: 2}
			if cursor != "" {
				c, err := knowledge.DecodeCursor(cursor)
				require.NoError(t, err)
				q.Before = c
			}
			page, err := store.ListPage(ctx, ws, q)
			require.NoError(t, err)
			if len(page.Documents) == 0 {
				break
			}
			for _, d := range page.Documents {
				seen[d.ID]++
				require.Equal(t, ws, d.WorkspaceID,
					"another workspace's document leaked into the page")
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		require.Len(t, seen, total)
		for id, n := range seen {
			require.Equal(t, 1, n, "document %s returned %d times: the cursor repeats rows", id, n)
		}
		for _, id := range ids {
			require.Contains(t, seen, id, "document %s was never returned: the cursor skipped a row", id)
		}
		require.NotContains(t, seen, foreign, "a foreign document must never be returned")
	})

	t.Run("kind filter is applied in SQL", func(t *testing.T) {
		guide := seedKnowledge(t, admin, ws, "style_guide", "the guide", base, 0.1)
		kind := knowledge.KindStyleGuide
		page, err := store.ListPage(ctx, ws, knowledge.ListQuery{Kind: &kind, Limit: 50})
		require.NoError(t, err)
		require.Len(t, page.Documents, 1)
		require.Equal(t, guide, page.Documents[0].ID)
	})
}

// TestKnowledgeStoreIsConfinedByRLS drives the production store over the runtime
// pool and proves the confinement is PostgreSQL's rather than a WHERE clause in
// Go: an operation naming another tenant's document affects nothing.
func TestKnowledgeStoreIsConfinedByRLS(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	wsA := knowledgeTenant(t, admin, "knowledge rls A")
	wsB := knowledgeTenant(t, admin, "knowledge rls B")
	store := postgres.NewKnowledgeStore(taskRuntimeDB(t))
	ctx := context.Background()

	docB := seedKnowledge(t, admin, wsB, "document", "belongs to B", time.Now().UTC(), 0.5)

	t.Run("get is not found across the boundary", func(t *testing.T) {
		_, err := store.Get(ctx, wsA, docB)
		require.ErrorIs(t, err, knowledge.ErrNotFound,
			"workspace A must not read workspace B's document")
	})

	t.Run("update affects nothing across the boundary", func(t *testing.T) {
		stolen := &knowledge.Document{ID: docB, WorkspaceID: wsA, Kind: knowledge.KindDocument,
			Title: "stolen", Content: "stolen"}
		_, err := store.Update(ctx, stolen)
		require.Error(t, err, "workspace A must not update workspace B's document")

		var title string
		require.NoError(t, admin.QueryRow(
			`SELECT title FROM knowledge_documents WHERE id = $1`, docB).Scan(&title))
		require.Equal(t, "belongs to B", title, "the row must be untouched")
	})

	t.Run("delete affects nothing across the boundary", func(t *testing.T) {
		err := store.Delete(ctx, wsA, docB)
		require.Error(t, err, "workspace A must not delete workspace B's document")

		var still int
		require.NoError(t, admin.QueryRow(
			`SELECT count(*) FROM knowledge_documents WHERE id = $1`, docB).Scan(&still))
		require.Equal(t, 1, still)
	})

	t.Run("listing and search are confined", func(t *testing.T) {
		page, err := store.ListPage(ctx, wsA, knowledge.ListQuery{Limit: knowledge.MaxPageSize})
		require.NoError(t, err)
		for _, d := range page.Documents {
			require.Equal(t, wsA, d.WorkspaceID, "the listing leaked a document owned by %s", d.WorkspaceID)
		}

		results, err := store.Search(ctx, wsA, make([]float32, knowledge.DefaultEmbeddingDimensions),
			nil, 50)
		require.NoError(t, err)
		for _, d := range results {
			require.Equal(t, wsA, d.WorkspaceID,
				"the similarity search leaked a document owned by %s", d.WorkspaceID)
		}
	})
}

// TestKnowledgeStoreUpdateIsNotAnUpsert pins the reason Update exists as its own
// method. Routing an update through INSERT … ON CONFLICT (id) would resurrect a
// document deleted between the caller's read and the write, turning a concurrent
// delete into a silent recreate.
func TestKnowledgeStoreUpdateIsNotAnUpsert(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	ws := knowledgeTenant(t, admin, "knowledge update")
	store := postgres.NewKnowledgeStore(taskRuntimeDB(t))
	ctx := context.Background()

	created, err := store.Upsert(ctx, &knowledge.Document{
		WorkspaceID: ws, Kind: knowledge.KindDocument, Title: "original", Content: "body",
		Embedding: make([]float32, knowledge.DefaultEmbeddingDimensions),
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, created.ID)

	// Delete it behind the service's back, then try to update it.
	require.NoError(t, store.Delete(ctx, ws, created.ID))

	_, err = store.Update(ctx, &knowledge.Document{
		ID: created.ID, WorkspaceID: ws, Kind: knowledge.KindDocument,
		Title: "resurrected", Content: "body",
		Embedding: make([]float32, knowledge.DefaultEmbeddingDimensions),
	})
	require.ErrorIs(t, err, knowledge.ErrNotFound,
		"updating a deleted document must report not found, not recreate it")

	var still int
	require.NoError(t, admin.QueryRow(
		`SELECT count(*) FROM knowledge_documents WHERE id = $1`, created.ID).Scan(&still))
	require.Zero(t, still, "the deleted document must not have come back")
}

// TestKnowledgeStoreUpdatePreservesCreatedAt checks the timestamp contract
// against the real column: updated_at moves, created_at does not.
func TestKnowledgeStoreUpdatePreservesCreatedAt(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	ws := knowledgeTenant(t, admin, "knowledge timestamps")
	store := postgres.NewKnowledgeStore(taskRuntimeDB(t))
	ctx := context.Background()

	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	created, err := store.Upsert(ctx, &knowledge.Document{
		WorkspaceID: ws, Kind: knowledge.KindDocument, Title: "t", Content: "c",
		Embedding: make([]float32, knowledge.DefaultEmbeddingDimensions),
		CreatedAt: createdAt,
	})
	require.NoError(t, err)

	updated, err := store.Update(ctx, &knowledge.Document{
		ID: created.ID, WorkspaceID: ws, Kind: knowledge.KindCampaignRule,
		Title: "renamed", Content: "changed",
		Embedding: make([]float32, knowledge.DefaultEmbeddingDimensions),
	})
	require.NoError(t, err)
	require.True(t, updated.CreatedAt.Equal(createdAt),
		"created_at must not move: got %v want %v", updated.CreatedAt, createdAt)
	require.True(t, updated.UpdatedAt.After(updated.CreatedAt),
		"updated_at must move past created_at")
	require.Equal(t, knowledge.KindCampaignRule, updated.Kind)
	require.Equal(t, "renamed", updated.Title)
}

// TestKnowledgeSearchIsRankedByCosine proves the similarity query is a real
// ranking and not an arbitrary selection: the document whose vector is closest
// to the query must come first.
func TestKnowledgeSearchIsRankedByCosine(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	ws := knowledgeTenant(t, admin, "knowledge search")
	store := postgres.NewKnowledgeStore(taskRuntimeDB(t))
	ctx := context.Background()

	// Cosine similarity is scale-invariant: [9.0,9.01,...] and [0.1,0.11,...]
	// point in almost exactly the same direction and rank identically, so vectors
	// that differ only in magnitude prove nothing about the ranking. These differ
	// in angle instead. The query is weighted toward the first dimension, and the
	// three documents put progressively less of themselves there.
	//
	//   near: identical to the query            -> distance 0
	//   mid:  uniform                           -> cosine ~0.965
	//   far:  weighted away from dimension one  -> cosine ~0.841
	dims := knowledge.DefaultEmbeddingDimensions
	uniform := make([]float32, dims)
	for i := range uniform {
		uniform[i] = 1
	}
	near := make([]float32, dims)
	far := make([]float32, dims)
	near[0], far[0] = 1, 0.1
	for i := 1; i < dims; i++ {
		near[i], far[i] = 0.5, 2
	}
	query := append([]float32(nil), near...)

	at := time.Now().UTC()
	docFar := seedKnowledgeVec(t, admin, ws, "far", at, far)
	docNear := seedKnowledgeVec(t, admin, ws, "near", at, near)
	docMid := seedKnowledgeVec(t, admin, ws, "mid", at, uniform)

	results, err := store.Search(ctx, ws, query, nil, 3)
	require.NoError(t, err)
	require.Len(t, results, 3)
	require.Equal(t, docNear, results[0].ID, "the nearest document must rank first")
	require.Equal(t, docMid, results[1].ID, "ranking must be by angle, not insertion order")
	require.Equal(t, docFar, results[2].ID, "the furthest document must rank last")

	t.Run("limit bounds the result set", func(t *testing.T) {
		results, err := store.Search(ctx, ws, query, nil, 1)
		require.NoError(t, err)
		require.Len(t, results, 1, "the limit must be honoured by the query")
		require.Equal(t, docNear, results[0].ID)
	})
}

// TestKnowledgeCreateRunsAsTheRuntimeRole is the complement to the isolation
// assertions: the write path works under the unprivileged role and lands in the
// right tenant. Without it, every test above would also pass if inserts simply
// failed.
func TestKnowledgeCreateRunsAsTheRuntimeRole(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	ws := knowledgeTenant(t, admin, "knowledge runtime write")

	svc := knowledge.NewService(postgres.NewKnowledgeStore(taskRuntimeDB(t)),
		knowledge.NewGatewayEmbedder(ai.StubProvider{}), nil, nil, 0)
	ctx := context.Background()

	created, err := svc.Create(ctx, ws, knowledge.KindStyleGuide, "Brand voice", "Write plainly.")
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, created.ID)
	require.NotEmpty(t, created.Embedding, "the gateway must have produced a vector")

	got, err := svc.Get(ctx, ws, created.ID)
	require.NoError(t, err)
	require.Equal(t, "Brand voice", got.Title)
	require.Equal(t, knowledge.KindStyleGuide, got.Kind)

	var storedWs uuid.UUID
	require.NoError(t, admin.QueryRow(
		`SELECT workspace_id FROM knowledge_documents WHERE id = $1`, created.ID).Scan(&storedWs))
	require.Equal(t, ws, storedWs, "the document must belong to the workspace it was created in")
}

// TestKnowledgeRuntimeRoleHasNoPrivilegedEscape asserts the properties that make
// the isolation above meaningful, specifically for knowledge_documents.
func TestKnowledgeRuntimeRoleHasNoPrivilegedEscape(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	var superuser, bypass, owns bool
	require.NoError(t, admin.QueryRow(`
		SELECT r.rolsuper, r.rolbypassrls,
		       EXISTS(SELECT 1 FROM pg_class c
		              JOIN pg_roles o ON o.oid = c.relowner
		              WHERE c.relname = 'knowledge_documents' AND o.rolname = r.rolname)
		FROM pg_roles r WHERE r.rolname = 'austro_app'`).
		Scan(&superuser, &bypass, &owns))
	require.False(t, superuser, "the runtime role must not be a superuser")
	require.False(t, bypass, "the runtime role must not have BYPASSRLS")
	require.False(t, owns, "the runtime role must not own knowledge_documents")

	var rls, forced bool
	require.NoError(t, admin.QueryRow(`
		SELECT relrowsecurity, relforcerowsecurity
		FROM pg_class WHERE relname = 'knowledge_documents'`).Scan(&rls, &forced))
	require.True(t, rls, "row-level security must be enabled")
	require.True(t, forced,
		"row-level security must be forced, so it applies to the table owner too")

	var policies int
	require.NoError(t, admin.QueryRow(`
		SELECT count(*) FROM pg_policy WHERE polrelid = 'knowledge_documents'::regclass`).Scan(&policies))
	require.Equal(t, 1, policies,
		"knowledge_documents must have exactly the one workspace isolation policy")
}

// TestKnowledgePaginationIndexExists checks the index that makes the bounded
// listing cheap. Without it every page is a full scan plus a sort, which is a
// performance cliff rather than a correctness bug -- and therefore easy to lose
// silently.
func TestKnowledgePaginationIndexExists(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	var found bool
	require.NoError(t, admin.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM pg_class c
			JOIN pg_index i ON i.indexrelid = c.oid
			WHERE i.indrelid = 'knowledge_documents'::regclass
			  AND c.relname = 'idx_knowledge_workspace_created')`).Scan(&found))
	require.True(t, found,
		"idx_knowledge_workspace_created must exist to back the bounded, newest-first listing")
}
