package austro_os_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// grantKnowledgeToRestrictedRoles grants the restricted isolation roles DML on
// the knowledge_documents table (additive setup via the admin connection).
func grantKnowledgeToRestrictedRoles(t *testing.T, admin *sql.DB, roles ...string) {
	t.Helper()
	for _, r := range roles {
		_, err := admin.Exec("GRANT SELECT, INSERT, UPDATE, DELETE ON knowledge_documents TO " + r)
		require.NoError(t, err)
	}
}

// seedKnowledgeRow inserts a deterministic knowledge_documents row via the
// admin (bypassing RLS) so the isolation boundary can be observed from the
// restricted roles' perspective.
func seedKnowledgeRow(t *testing.T, admin *sql.DB, id, ws uuid.UUID, kind, title, content string) {
	t.Helper()
	_, err := admin.Exec(`INSERT INTO knowledge_documents (id, workspace_id, kind, title, content, embedding)
		VALUES ($1,$2,$3,$4,$5,'[1,0,0,0,0,0,0,0,0,0]'::vector)
		ON CONFLICT (id) DO NOTHING`, id, ws, kind, title, content)
	require.NoError(t, err)
}

// TestKnowledgeRLSPolicyEnabled verifies knowledge_documents carries the
// standard workspace isolation policy and has row-level security enabled
// (added additively by the Phase 2 migration).
func TestKnowledgeRLSPolicyEnabled(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()

	var n int
	require.NoError(t, admin.QueryRow(
		`SELECT count(*) FROM pg_policy WHERE polname='workspace_isolation_policy' AND polrelid='knowledge_documents'::regclass`,
	).Scan(&n))
	require.Equal(t, 1, n, "expected workspace_isolation_policy on knowledge_documents")

	var rlsEnabled bool
	require.NoError(t, admin.QueryRow(`SELECT relrowsecurity FROM pg_class WHERE oid='knowledge_documents'::regclass`).Scan(&rlsEnabled))
	require.True(t, rlsEnabled, "row-level security must be enabled on knowledge_documents")
}

// TestKnowledgeWorkspaceRLSIsolation proves knowledge_documents honour
// row-level security from the perspective of the restricted non-bypass roles.
func TestKnowledgeWorkspaceRLSIsolation(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))
	grantKnowledgeToRestrictedRoles(t, admin, workspaceARole, workspaceBRole)

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	docA, docB := uuid.New(), uuid.New()
	seedKnowledgeRow(t, admin, docA, wsA, "document", "A doc", "A content")
	seedKnowledgeRow(t, admin, docB, wsB, "document", "B doc", "B content")
	t.Cleanup(func() {
		_, _ = admin.Exec(`DELETE FROM knowledge_documents WHERE id IN ($1, $2)`, docA, docB)
	})

	t.Run("A sees only A documents", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		aSeen, bSeen := 0, 0
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM knowledge_documents WHERE id=$1`, docA).Scan(&aSeen))
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM knowledge_documents WHERE id=$1`, docB).Scan(&bSeen))
		require.Equal(t, 1, aSeen, "A must see its own document")
		require.Zero(t, bSeen, "A must NOT see B's document")
	})

	t.Run("B sees only B documents", func(t *testing.T) {
		dbB := connectRestricted(t, workspaceBRole)
		defer dbB.Close()
		setWorkspace(t, dbB, workspaceB)
		aSeen, bSeen := 0, 0
		require.NoError(t, dbB.QueryRow(`SELECT count(*) FROM knowledge_documents WHERE id=$1`, docA).Scan(&aSeen))
		require.NoError(t, dbB.QueryRow(`SELECT count(*) FROM knowledge_documents WHERE id=$1`, docB).Scan(&bSeen))
		require.Zero(t, aSeen, "B must NOT see A's document")
		require.Equal(t, 1, bSeen, "B must see its own document")
	})

	t.Run("A cannot modify B documents", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		res, err := dbA.Exec(`UPDATE knowledge_documents SET title='HACKED' WHERE id=$1`, docB)
		require.NoError(t, err)
		aff, _ := res.RowsAffected()
		require.Zero(t, aff, "A must not update B's document")
	})

	t.Run("no workspace context reveals no documents", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		var n int
		require.NoError(t, dbA.QueryRow(`SELECT count(*) FROM knowledge_documents`).Scan(&n))
		require.Zero(t, n, "without a workspace context no documents may be visible")
	})

	t.Run("similarity search stays within workspace", func(t *testing.T) {
		dbA := connectRestricted(t, workspaceARole)
		defer dbA.Close()
		setWorkspace(t, dbA, workspaceA)
		var bSeen int
		require.NoError(t, dbA.QueryRow(
			`SELECT count(*) FROM knowledge_documents WHERE id=$1 AND embedding <=> '[1,0,0,0,0,0,0,0,0,0]'::vector < 2`,
			docB).Scan(&bSeen))
		require.Zero(t, bSeen, "similarity search must not expose B's document to A")
	})
}
