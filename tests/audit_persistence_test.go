package austro_os_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"austro-os/infrastructure/auditstore"
	"austro-os/infrastructure/database"
	"austro-os/infrastructure/postgres"
	"austro-os/internal/api"
	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/config"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests prove persistent audit against a real PostgreSQL.
//
// They deliberately do not accept an in-memory sink as evidence: every
// assertion reads rows back out of audit_events, and the restart test builds a
// second writer instance so the chain has to be recovered from the database
// rather than from a process's memory.

const (
	auditWorkspaceA = "77777777-0000-4000-8000-00000000000a"
	auditWorkspaceB = "77777777-0000-4000-8000-00000000000b"
)

func auditFixture(t *testing.T) (*database.Topology, *auditstore.Store, *sql.DB) {
	t.Helper()
	topo := topologyFixture(t)
	admin := adminDB(t, topo)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := auditstore.New(ctx, admin)
	require.NoError(t, err)
	return topo, store, admin
}

// A, B, G. Appending through the writer persists rows, in chain order, and the
// persisted chain verifies.
func TestAuditEventsArePersistedAndChainVerifies(t *testing.T) {
	_, store, admin := auditFixture(t)

	before := auditRowCount(t, admin)
	var appended []*audit.AuditEvent
	for i, outcome := range []string{"success", "denied"} {
		ev, err := store.Append(context.Background(), audit.Record{
			EventType:  "test.persisted",
			ActorType:  "user",
			TargetType: "test",
			Outcome:    outcome,
			Principle:  "Security by Design",
			Details:    map[string]any{"index": i},
		})
		require.NoError(t, err)
		appended = append(appended, ev)
	}

	after := auditRowCount(t, admin)
	require.Equal(t, before+2, after, "each append must persist exactly one row")

	chain, err := store.Chain(context.Background())
	require.NoError(t, err)
	require.True(t, audit.VerifyHashChain(chain), "the persisted chain must verify")

	// The chain must be a single line, not a fork: every non-genesis event's
	// parent must be the immediately preceding event.
	require.True(t, chain[0].Genesis, "the chain must start at a genesis event")
	for i := 1; i < len(chain); i++ {
		require.Equal(t, chain[i-1].ID, chain[i].HashParent,
			"event %d must link to its predecessor", i)
	}
	require.Equal(t, appended[1].EventID, chain[len(chain)-1].EventID,
		"the last persisted row must be the last appended event")

	ok, err := store.Verify(context.Background())
	require.NoError(t, err)
	require.True(t, ok, "Verify must succeed against the persisted records")
}

// C. Tampering with a persisted row is detected.
func TestAuditTamperingIsDetected(t *testing.T) {
	_, store, _ := auditFixture(t)

	ev, err := store.Append(context.Background(), audit.Record{
		EventType: "test.tamper", ActorType: "user", TargetType: "test",
		Outcome: "denied", Principle: "Security by Design",
	})
	require.NoError(t, err)

	ok, err := store.Verify(context.Background())
	require.NoError(t, err)
	require.True(t, ok, "the chain must verify before tampering")

	// Rewrite the outcome of a recorded failure into a success. This is the
	// attack the chain exists to stop, and it is done with the owner
	// credential, i.e. with every privilege the database can grant. Neither the
	// runtime role nor the administrative role could perform it: both are
	// append-only on this table.
	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()
	res, err := owner.Exec(`UPDATE audit_events SET outcome = 'success' WHERE event_id = $1`, ev.EventID)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)

	ok, err = store.Verify(context.Background())
	require.NoError(t, err)
	require.False(t, ok, "rewriting the outcome of an audited event must break verification")

	// Restore, so later tests start from an intact chain.
	_, err = owner.Exec(`UPDATE audit_events SET outcome = 'denied' WHERE event_id = $1`, ev.EventID)
	require.NoError(t, err)
	ok, err = store.Verify(context.Background())
	require.NoError(t, err)
	require.True(t, ok, "restoring the original value must restore verification")
}

// D. Workspace isolation applies to audit rows.
func TestAuditRowsAreWorkspaceIsolated(t *testing.T) {
	topo, store, _ := auditFixture(t)
	seedWorkspace(t, topo, auditWorkspaceA, "Audit A")
	seedWorkspace(t, topo, auditWorkspaceB, "Audit B")

	wsA := uuid.MustParse(auditWorkspaceA)
	wsB := uuid.MustParse(auditWorkspaceB)

	for _, ws := range []*uuid.UUID{&wsA, &wsB} {
		_, err := store.Append(context.Background(), audit.Record{
			EventType: "test.scoped", ActorType: "user", TargetType: "test",
			Outcome: "success", Principle: "Security by Design", WorkspaceID: ws,
		})
		require.NoError(t, err)
	}

	rt := runtimeDB(t, topo)
	for _, tc := range []struct{ bound, other string }{
		{auditWorkspaceA, auditWorkspaceB},
		{auditWorkspaceB, auditWorkspaceA},
	} {
		tx := bindWorkspace(t, rt, tc.bound)
		var other int
		require.NoError(t, tx.QueryRow(
			`SELECT count(*) FROM audit_events WHERE workspace_id = $1`, tc.other).Scan(&other))
		require.Zero(t, other, "a session bound to one workspace must not read another workspace's audit rows")

		var own int
		require.NoError(t, tx.QueryRow(
			`SELECT count(*) FROM audit_events WHERE workspace_id = $1`, tc.bound).Scan(&own))
		require.GreaterOrEqual(t, own, 1, "a session must still see its own workspace's audit rows")
	}
}

// E. When durable audit cannot be written, a security-sensitive operation must
// not report success.
//
// This is driven through the real authentication handler over HTTP, with INSERT
// on audit_events revoked from the administrative role so persistence genuinely
// fails rather than being simulated.
func TestLoginFailsClosedWhenAuditCannotBePersisted(t *testing.T) {
	topo, store, admin := auditFixture(t)
	rt := runtimeDB(t, topo)
	seedWorkspace(t, topo, auditWorkspaceA, "Audit A")
	seedAuditUser(t, topo, "audit-login-user", "correct horse battery", auditWorkspaceA)

	handler := newTestAuthHandler(t, rt, store)

	// Baseline: with audit working, login succeeds and leaves a row.
	before := auditRowCount(t, admin)
	rec := httptest.NewRecorder()
	handler.Login(rec, loginRequest(t, "audit-login-user", "correct horse battery"))
	require.Equal(t, http.StatusOK, rec.Code, "login must succeed while audit is healthy")
	require.Equal(t, before+1, auditRowCount(t, admin), "a successful login must persist an audit row")

	// Remove the writer's ability to insert. The owner connection is used
	// because only it can revoke from the administrative role.
	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()
	_, err = owner.Exec(`REVOKE INSERT ON audit_events FROM ` + topo.Admin)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = owner.Exec(`GRANT SELECT, INSERT ON audit_events TO ` + topo.Admin)
	})

	rec = httptest.NewRecorder()
	handler.Login(rec, loginRequest(t, "audit-login-user", "correct horse battery"))
	require.NotEqual(t, http.StatusOK, rec.Code,
		"login must not report success when its audit record could not be persisted")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// F. The chain survives a restart: a second writer instance recovers the head
// from the database and continues the same chain instead of forking a new one.
func TestAuditChainSurvivesRestart(t *testing.T) {
	_, store, admin := auditFixture(t)

	_, err := store.Append(context.Background(), audit.Record{
		EventType: "test.pre-restart", ActorType: "user", TargetType: "test",
		Outcome: "success", Principle: "Security by Design",
	})
	require.NoError(t, err)
	headBefore := store.Head()
	require.NotNil(t, headBefore)

	// A brand new Store, as a restarted process would construct.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	restarted, err := auditstore.New(ctx, admin)
	require.NoError(t, err)
	require.NotNil(t, restarted.Head(), "a restarted writer must recover the chain head")
	require.Equal(t, headBefore.EventID, restarted.Head().EventID,
		"the recovered head must be the last persisted event")

	genesisCount := auditGenesisCount(t, admin)
	require.Equal(t, 1, genesisCount,
		"a restart must never create a second genesis and fork the chain")

	after, err := restarted.Append(context.Background(), audit.Record{
		EventType: "test.post-restart", ActorType: "user", TargetType: "test",
		Outcome: "success", Principle: "Security by Design",
	})
	require.NoError(t, err)
	require.Equal(t, headBefore.ID, after.HashParent,
		"the first event after a restart must link to the pre-restart head")

	chain, err := restarted.Chain(context.Background())
	require.NoError(t, err)
	require.True(t, audit.VerifyHashChain(chain), "the chain must still verify across the restart")
	require.Equal(t, genesisCount, auditGenesisCount(t, admin))
}

// H. Secrets are never persisted, even when a caller passes them in details.
func TestAuditNeverPersistsSecrets(t *testing.T) {
	_, store, admin := auditFixture(t)

	const password = "sup3r-s3cret-value"
	const token = "eyJhbGciOiJIUzI1NiJ9.secret-payload"
	ev, err := store.Append(context.Background(), audit.Record{
		EventType: "test.redaction", ActorType: "user", TargetType: "test",
		Outcome: "success", Principle: "Security by Design",
		Details: map[string]any{
			"username":       "some-user",
			"password":       password,
			"refresh_token":  token,
			"jwt":            token,
			"Authorization":  "Bearer " + token,
			"api_key":        "ak-" + password,
			"session_cookie": "sid=" + token,
			"reason":         "not a secret",
		},
	})
	require.NoError(t, err)

	var raw []byte
	require.NoError(t, admin.QueryRow(
		`SELECT outcome_details::text FROM audit_events WHERE event_id = $1`, ev.EventID).Scan(&raw))
	persisted := string(raw)

	require.Contains(t, persisted, "some-user", "non-secret context must still be recorded")
	require.Contains(t, persisted, "not a secret")
	require.NotContains(t, persisted, password, "a password value must never be persisted")
	require.NotContains(t, persisted, token, "a token value must never be persisted")
	require.Contains(t, persisted, "[redacted]", "redacted keys must be marked, not silently dropped")
}

// I. Concurrent appends keep the chain linear: no forked parents, no lost rows.
func TestConcurrentAuditAppendsKeepChainLinear(t *testing.T) {
	_, store, admin := auditFixture(t)

	const writers = 16
	// Counted as a delta, not an absolute: audit_events is append-only and
	// survives between runs, so an absolute count would grow every time and
	// make this test fail on a warm database.
	before := auditCountByType(t, admin, "test.concurrent")
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.Append(context.Background(), audit.Record{
				EventType: "test.concurrent", ActorType: "user", TargetType: "test",
				Outcome: "success", Principle: "Security by Design",
				Details: map[string]any{"writer": i},
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "concurrent appends must all succeed")
	}

	chain, err := store.Chain(context.Background())
	require.NoError(t, err)
	require.True(t, audit.VerifyHashChain(chain), "the chain must verify after concurrent appends")

	// Every parent must appear exactly once as a parent: a repeated parent
	// means two events claimed the same predecessor, i.e. the chain forked.
	parents := map[uuid.UUID]int{}
	for _, ev := range chain[1:] {
		parents[ev.HashParent]++
	}
	for parent, n := range parents {
		require.Equal(t, 1, n, "parent %s was claimed by %d events; the chain forked", parent, n)
	}

	after := auditCountByType(t, admin, "test.concurrent")
	require.Equal(t, writers, after-before, "no concurrent append may be lost")
}

// The runtime role must be append-only on the audit table: it cannot rewrite or
// delete the evidence it produces.
func TestAuditTableIsAppendOnlyForRuntimeRole(t *testing.T) {
	topo, store, _ := auditFixture(t)
	rt := runtimeDB(t, topo)

	_, err := store.Append(context.Background(), audit.Record{
		EventType: "test.appendonly", ActorType: "user", TargetType: "test",
		Outcome: "success", Principle: "Security by Design",
	})
	require.NoError(t, err)

	tx := bindWorkspace(t, rt, auditWorkspaceA)
	_, err = tx.Exec(`UPDATE audit_events SET outcome = 'tampered'`)
	require.Error(t, err, "the runtime role must not be able to update audit rows")

	tx2 := bindWorkspace(t, rt, auditWorkspaceA)
	_, err = tx2.Exec(`DELETE FROM audit_events`)
	require.Error(t, err, "the runtime role must not be able to delete audit rows")

	// The administrative role appends and reads the chain, but it cannot
	// rewrite history either: only a superuser or the table owner can, which is
	// why the chain is what makes a rewrite detectable.
	admin := adminDB(t, topo)
	_, err = admin.Exec(`UPDATE audit_events SET outcome = 'tampered'`)
	require.Error(t, err, "the admin role must not be able to update audit rows")
	_, err = admin.Exec(`DELETE FROM audit_events`)
	require.Error(t, err, "the admin role must not be able to delete audit rows")
}

// --- helpers -------------------------------------------------------------

func auditRowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&n))
	return n
}

func auditCountByType(t *testing.T, db *sql.DB, eventType string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM audit_events WHERE event_type = $1`, eventType).Scan(&n))
	return n
}

func auditGenesisCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM audit_events WHERE genesis = TRUE`).Scan(&n))
	return n
}

// seedAuditUser creates a workspace member with a known password, through the
// owner connection as a deployment would.
func seedAuditUser(t *testing.T, topo *database.Topology, username, password, workspaceID string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	require.NoError(t, err)
	owner, err := sql.Open("pgx", getEnv().postgresDSN)
	require.NoError(t, err)
	defer owner.Close()
	_, err = owner.Exec(`INSERT INTO users (id, username, password_hash, display_name, is_founder, role, workspace_id)
		VALUES ($1,$2,$3,$4,FALSE,'workspace_member',$5)
		ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash, role = 'workspace_member', workspace_id = $5`,
		uuid.NewSHA1(uuid.NameSpaceDNS, []byte(username)).String(), username, hash, username, workspaceID)
	require.NoError(t, err)
}

// newTestAuthHandler builds the production authentication handler against the
// runtime database pool and the persistent audit store.
func newTestAuthHandler(t *testing.T, rt *sql.DB, sink audit.Sink) *api.AuthHandler {
	t.Helper()
	cfg := &config.Config{
		PostgresDSN:        getEnv().postgresDSN,
		PostgresRuntimeDSN: "postgres://runtime@db:5432/austro?sslmode=disable",
		RedisAddr:          "redis:6379",
		RabbitMQURL:        "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:          "test-access-secret-1234567890-abcdef",
		JWTRefreshSecret:   "test-refresh-secret-1234567890-abcdef",
		Environment:        "test",
	}
	require.NoError(t, cfg.Validate())
	return api.NewAuthHandler(cfg, auth.Initialize(cfg), postgres.NewUserStore(rt)).SetAuditSink(sink)
}

func loginRequest(t *testing.T, username, password string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}
