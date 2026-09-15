package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type failingCellStore struct {
	*fakeRepo
	err error
}

func (f *failingCellStore) StoreSessionMemory(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return f.err
}

type failingMemoryAudit struct {
	err error
}

func (f failingMemoryAudit) Record(context.Context, AuditRecord)            {}
func (f failingMemoryAudit) RecordError(context.Context, AuditRecord) error { return f.err }

func TestBankRejectsInvalidLayersAndNamespaceCollisions(t *testing.T) {
	bank := NewBank(newFakeRepo(), nil, nil)
	ws := uuid.New()

	require.ErrorIs(t, bank.Write(context.Background(), ws, MemoryLayer(99), "key", []byte("value"), time.Minute), ErrInvalidLayer)
	_, err := bank.Read(context.Background(), ws, MemoryLayer(99), "key")
	require.ErrorIs(t, err, ErrInvalidLayer)

	for _, key := range []string{"workspace:" + ws.String() + ":session:key", "org:key", " key", "key "} {
		require.ErrorIs(t, bank.Write(context.Background(), ws, LayerWorkspace, key, []byte("value"), time.Minute), ErrInvalidKey,
			"partition-shaped or whitespace-padded keys must not collide with generated namespaces")
	}
}

func TestBankFailedStorageLeavesNoCell(t *testing.T) {
	storeErr := errors.New("redis unavailable")
	repo := &failingCellStore{fakeRepo: newFakeRepo(), err: storeErr}
	bank := NewBank(repo, nil, nil)
	ws := uuid.New()

	require.ErrorIs(t, bank.Write(context.Background(), ws, LayerSession, "failed", []byte("value"), time.Minute), storeErr)
	_, exists := repo.session["workspace:"+ws.String()+":session:failed"]
	require.False(t, exists, "a failed storage call must not leave an orphan cell")
}

func TestBankFailedSuccessAuditRollsBackCell(t *testing.T) {
	repo := newFakeRepo()
	auditErr := errors.New("audit unavailable")
	bank := NewBank(repo, failingMemoryAudit{err: auditErr}, nil)
	ws := uuid.New()

	require.ErrorIs(t, bank.Write(context.Background(), ws, LayerWorkspace, "unaudited", []byte("value"), time.Minute), auditErr)
	_, exists := repo.workspace["workspace:"+ws.String()+":workspace:unaudited"]
	require.False(t, exists, "a successful storage call must be rolled back when success audit persistence fails")
}
