package memory

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakeRepo implements only the memory methods of repository.Repository. A value
// is stored per (layer, key) with the given TTL; reads/delete honour the exact
// key (so partition-scoping is observable in isolation tests).
type fakeRepo struct {
	session   map[string][]byte
	longTerm  map[string][]byte
	workspace map[string][]byte
	org       map[string][]byte
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		session:   map[string][]byte{},
		longTerm:  map[string][]byte{},
		workspace: map[string][]byte{},
		org:       map[string][]byte{},
	}
}

func (f *fakeRepo) store(m map[string][]byte, key string, v []byte) error {
	m[key] = v
	return nil
}
func (f *fakeRepo) get(m map[string][]byte, key string) ([]byte, error) {
	v, ok := m[key]
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}
func (f *fakeRepo) del(m map[string][]byte, key string) error {
	delete(m, key)
	return nil
}

func (f *fakeRepo) StoreSessionMemory(_ context.Context, k string, v []byte, _ time.Duration) error {
	return f.store(f.session, k, v)
}
func (f *fakeRepo) RetrieveSessionMemory(_ context.Context, k string) ([]byte, error) {
	return f.get(f.session, k)
}
func (f *fakeRepo) DeleteSessionMemory(_ context.Context, k string) error { return f.del(f.session, k) }
func (f *fakeRepo) ListSessionMemoryPrefix(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (f *fakeRepo) StoreLongTermMemory(_ context.Context, k string, v []byte, _ time.Duration) error {
	return f.store(f.longTerm, k, v)
}
func (f *fakeRepo) RetrieveLongTermMemory(_ context.Context, k string) ([]byte, error) {
	return f.get(f.longTerm, k)
}
func (f *fakeRepo) DeleteLongTermMemory(_ context.Context, k string) error {
	return f.del(f.longTerm, k)
}
func (f *fakeRepo) ListLongTermMemoryPrefix(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (f *fakeRepo) StoreWorkspaceMemory(_ context.Context, k string, v []byte, _ time.Duration) error {
	return f.store(f.workspace, k, v)
}
func (f *fakeRepo) RetrieveWorkspaceMemory(_ context.Context, k string) ([]byte, error) {
	return f.get(f.workspace, k)
}
func (f *fakeRepo) DeleteWorkspaceMemory(_ context.Context, k string) error {
	return f.del(f.workspace, k)
}
func (f *fakeRepo) ListWorkspaceMemoryPrefix(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (f *fakeRepo) StoreOrganizationalMemory(_ context.Context, k string, v []byte, _ time.Duration) error {
	return f.store(f.org, k, v)
}
func (f *fakeRepo) RetrieveOrganizationalMemory(_ context.Context, k string) ([]byte, error) {
	return f.get(f.org, k)
}
func (f *fakeRepo) DeleteOrganizationalMemory(_ context.Context, k string) error {
	return f.del(f.org, k)
}
func (f *fakeRepo) ListOrganizationalMemoryPrefix(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func TestBankWorkspacePartitionIsolation(t *testing.T) {
	repo := newFakeRepo()
	bank := NewBank(repo, nil, nil)
	ctx := context.Background()
	wsA := uuid.New()
	wsB := uuid.New()

	require.NoError(t, bank.Write(ctx, wsA, LayerSession, "key", []byte("A-value"), time.Minute))
	require.NoError(t, bank.Write(ctx, wsB, LayerSession, "key", []byte("B-value"), time.Minute))

	// A reads only A's value; B reads only B's — same logical key, distinct partitions.
	vA, err := bank.Read(ctx, wsA, LayerSession, "key")
	require.NoError(t, err)
	require.Equal(t, "A-value", string(vA))
	vB, err := bank.Read(ctx, wsB, LayerSession, "key")
	require.NoError(t, err)
	require.Equal(t, "B-value", string(vB))

	// The backing key explicitly carries the workspace partition.
	_, aExists := repo.session["workspace:"+wsA.String()+":session:key"]
	require.True(t, aExists, "A memory must be partitioned under A's workspace key")
	_, bExists := repo.session["workspace:"+wsB.String()+":session:key"]
	require.True(t, bExists, "B memory must be partitioned under B's workspace key")
}

func TestBankOrganizationalSharedAcrossWorkspaces(t *testing.T) {
	repo := newFakeRepo()
	bank := NewBank(repo, nil, nil)
	wsA := uuid.New()
	wsB := uuid.New()

	require.NoError(t, bank.Write(context.Background(), wsA, LayerOrganizational, "brand", []byte("tone"), time.Minute))
	// B (different workspace) reads the same org-wide cell without collision.
	v, err := bank.Read(context.Background(), wsB, LayerOrganizational, "brand")
	require.NoError(t, err)
	require.Equal(t, "tone", string(v))
}

func TestBankValidation(t *testing.T) {
	bank := NewBank(newFakeRepo(), nil, nil)
	ctx := context.Background()
	ws := uuid.New()

	t.Run("nil workspace denied", func(t *testing.T) {
		require.ErrorIs(t, bank.Write(ctx, uuid.Nil, LayerSession, "k", []byte("v"), time.Minute), ErrWorkspaceRequired)
	})
	t.Run("oversized value rejected", func(t *testing.T) {
		big := make([]byte, maxValueBytes+1)
		require.ErrorIs(t, bank.Write(ctx, ws, LayerSession, "k", big, time.Minute), ErrValueTooLarge)
	})
	t.Run("secret content rejected", func(t *testing.T) {
		require.ErrorIs(t, bank.Write(ctx, ws, LayerSession, "k", []byte("sk-abc123secret"), time.Minute), ErrSecretRejected)
	})
	t.Run("negative ttl rejected", func(t *testing.T) {
		require.ErrorIs(t, bank.Write(ctx, ws, LayerSession, "k", []byte("v"), -1*time.Second), ErrInvalidTTL)
	})
	t.Run("invalid key rejected", func(t *testing.T) {
		require.ErrorIs(t, bank.Write(ctx, ws, LayerSession, "", []byte("v"), time.Minute), ErrInvalidKey)
	})
	t.Run("cross workspace read denied for missing", func(t *testing.T) {
		require.NoError(t, bank.Write(ctx, ws, LayerSession, "x", []byte("v"), time.Minute))
		other := uuid.New()
		_, err := bank.Read(ctx, other, LayerSession, "x")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

func TestBankAuditRollupAndTTLCap(t *testing.T) {
	repo := newFakeRepo()
	var recs []AuditRecord
	bank := NewBank(repo, captureSink{fn: func(r AuditRecord) { recs = append(recs, r) }}, nil)

	long := 3000 * time.Hour // > maxTTL (90 days)
	ctx := WithTrace(context.Background(), "trace-m", "span-m")
	require.NoError(t, bank.Write(ctx, uuid.New(), LayerLongTerm, "draft", []byte("content"), long))

	// Success audit recorded with principle + trace correlation.
	require.GreaterOrEqual(t, len(recs), 1)
	last := recs[len(recs)-1]
	require.Equal(t, "memory.write", last.EventType)
	require.Equal(t, "success", last.Outcome)
	require.Equal(t, "longterm", last.Layer)
	require.Equal(t, "trace-m", last.TraceID)
	require.Equal(t, "span-m", last.SpanID)
}

type captureSink struct{ fn func(AuditRecord) }

func (c captureSink) Record(_ context.Context, rec AuditRecord) { c.fn(rec) }

func TestSecretShapedRequiresATokenBoundaryForShortPrefixes(t *testing.T) {
	secrets := []string{
		"sk-abc123",
		"sk-live-0123456789abcdef",
		"credential: sk-proj-abcdef",
		"X-Api-Key: sk-abc",
		"token=pk-abc",
		"-----BEGIN RSA PRIVATE KEY-----",
		"authorization: bearer abcdef",
		"api_key=abc",
		"apikey=abc",
		"secret=abc",
	}
	for _, s := range secrets {
		require.Truef(t, isSecretShaped([]byte(s)), "must reject secret-shaped value %q", s)
	}

	// Ordinary prose that merely contains the letters "sk-"/"pk-" is not a
	// credential and must be storable.
	ordinary := []string{
		"task-list for the week",
		"risk-free launch plan",
		"desk-drawer inventory",
		"mask-out the logo",
		"ask-follow up tomorrow",
		"brisk-start to the meeting",
	}
	for _, s := range ordinary {
		require.Falsef(t, isSecretShaped([]byte(s)), "must accept ordinary value %q", s)
	}
}

func TestBankAcceptsOrdinaryValuesContainingSkSubstring(t *testing.T) {
	bank := NewBank(newFakeRepo(), nil, nil)
	ctx := context.Background()
	ws := uuid.New()

	require.NoError(t, bank.Write(ctx, ws, LayerWorkspace, "note", []byte("task-list for the week"), time.Minute))
	got, err := bank.Read(ctx, ws, LayerWorkspace, "note")
	require.NoError(t, err)
	require.Equal(t, "task-list for the week", string(got))
}
