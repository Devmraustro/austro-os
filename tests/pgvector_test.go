package austro_os_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ensureVectorTable idempotently provisions the workspace-isolated vector
// store, matching the already-approved schema used by the running system. It
// is required for the PGVector criterion (workspace-filtered similarity search).
func ensureVectorTable(t *testing.T, admin *sql.DB) {
	t.Helper()
	_, err := admin.Exec(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS memory_embeddings (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
			memory_id   TEXT NOT NULL,
			content     TEXT NOT NULL,
			embedding   vector(10)
		);
		ALTER TABLE memory_embeddings ENABLE ROW LEVEL SECURITY;
	`)
	require.NoError(t, err)

	// Unique constraint required by the idempotent ON CONFLICT (memory_id)
	// upsert in seedEmbeddings; the running schema may predate this index.
	_, err = admin.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_memory_embeddings_memory_id ON memory_embeddings(memory_id)`)
	require.NoError(t, err)

	var policyExists bool
	require.NoError(t, admin.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM pg_policy WHERE polname='workspace_isolation_policy' AND polrelid='memory_embeddings'::regclass)`).Scan(&policyExists))
	if !policyExists {
		_, err = admin.Exec(`CREATE POLICY workspace_isolation_policy ON memory_embeddings
			USING (workspace_id = current_setting('app.current_workspace', true)::uuid)`)
		require.NoError(t, err)
	}
	_, err = admin.Exec(`GRANT SELECT, INSERT, UPDATE, DELETE ON memory_embeddings TO ` + workspaceARole + `, ` + workspaceBRole)
	require.NoError(t, err)
}

// seedEmbeddings inserts two vectors per workspace (A and B) that are far
// apart so that similarity ranking is unambiguous.
func seedEmbeddings(t *testing.T, admin *sql.DB) {
	t.Helper()
	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)

	// Query vector strongly biased toward A so it ranks A's rows first.
	_rows := []struct {
		memID string
		vec   string
		ws    uuid.UUID
	}{
		{"mem-a1", "[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]", wsA},
		{"mem-a2", "[1.0, 0.9, 0.8, 0.7, 0.6, 0.5, 0.4, 0.3, 0.2, 0.1]", wsA},
		{"mem-b1", "[1.0, 1.0, 1.0, 1.0, 1.0, -1.0, -1.0, -1.0, -1.0, -1.0]", wsB},
		{"mem-b2", "[-1.0, -1.0, -1.0, -1.0, -1.0, 1.0, 1.0, 1.0, 1.0, 1.0]", wsB},
	}
	for _, r := range _rows {
		_, err := admin.Exec(`INSERT INTO memory_embeddings (workspace_id, memory_id, content, embedding)
			VALUES ($1, $2, $3, $4::vector)
			ON CONFLICT (memory_id) DO UPDATE SET embedding = EXCLUDED.embedding, content = EXCLUDED.content`,
			r.ws, r.memID, r.memID, r.vec)
		require.NoError(t, err)
	}
}

// TestPGVectorWorkspaceIsolation runs a REAL cosine similarity search over the
// live PGVector column and asserts (a) correct ranking and (b) that results
// never leak across workspace boundaries.
func TestPGVectorWorkspaceIsolation(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	ensureVectorTable(t, admin)
	seedEmbeddings(t, admin)

	queryVec := "[0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0]"

	t.Run("A search returns only A vectors sorted by similarity", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)

		rows, err := dbA.Query(fmt.Sprintf(
			`SELECT memory_id, 1 - (embedding <=> '%s'::vector) AS sim
			 FROM memory_embeddings ORDER BY embedding <=> '%s'::vector LIMIT 50`, queryVec, queryVec))
		require.NoError(t, err)
		defer rows.Close()

		var got []string
		var sims []float64
		for rows.Next() {
			var id string
			var sim float64
			require.NoError(t, rows.Scan(&id, &sim))
			got = append(got, id)
			sims = append(sims, sim)
		}
		// Only A's vectors must be returned.
		require.ElementsMatch(t, []string{"mem-a1", "mem-a2"}, got, "A search leaked non-A vectors or missed A vectors")

		// Rank order: mem-a1 is closest to the query (sim ~1.0), then mem-a2.
		require.Equal(t, "mem-a1", got[0], "most similar A vector must rank first")
		require.InDelta(t, 1.0, sims[0], 1e-6, "self-similarity must be ~1.0")
		require.True(t, sims[0] > sims[1], "ranking must be monotonically descending")
	})

	t.Run("B search returns only B vectors", func(t *testing.T) {
		dbB := connectRestricted(t, workspaceBRole)
		defer dbB.Close()
		setWorkspace(t, dbB, workspaceB)

		rows, err := dbB.Query(fmt.Sprintf(
			`SELECT memory_id FROM memory_embeddings ORDER BY embedding <=> '%s'::vector LIMIT 50`, queryVec))
		require.NoError(t, err)
		defer rows.Close()

		var got []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			got = append(got, id)
		}
		require.ElementsMatch(t, []string{"mem-b1", "mem-b2"}, got, "B search leaked A vectors or missed B vectors")
	})

	t.Run("A cannot see B vectors via similarity search", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)

		var seenB int
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM memory_embeddings WHERE memory_id IN ('mem-b1','mem-b2')`).Scan(&seenB))
		require.Zero(t, seenB, "A's similarity search must not expose B vectors")
	})
}
