package austro_os_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"austro-os/infrastructure/redis"
	"austro-os/internal/memory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// memoryAuditSink records Bank decisions for tracing assertions.
type memoryAuditSink struct {
	mu   sync.Mutex
	recs []memory.AuditRecord
}

func (a *memoryAuditSink) Record(_ context.Context, rec memory.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, rec)
}

func (a *memoryAuditSink) count(eventType, outcome string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, r := range a.recs {
		if r.EventType == eventType && r.Outcome == outcome {
			n++
		}
	}
	return n
}

// TestMemoryBankWorkspaceIsolationIntegration proves the workspace-scoped memory
// facade isolates reads/writes across two workspaces via the live Redis store.
func TestMemoryBankWorkspaceIsolationIntegration(t *testing.T) {
	rdb := redisClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, rdb.Ping(ctx).Err(), "must be able to reach Redis")

	store := redis.NewMemoryStore(rdb)
	audit := &memoryAuditSink{}
	bank := memory.NewBank(store, audit, nil)

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	memCtx := memory.WithTrace(context.Background(), "trace-mem", "span-mem")

	// Same logical key under two workspaces must hold distinct values.
	require.NoError(t, bank.Write(memCtx, wsA, memory.LayerSession, "greeting", []byte("hello-A"), time.Minute))
	require.NoError(t, bank.Write(memCtx, wsB, memory.LayerSession, "greeting", []byte("hello-B"), time.Minute))
	t.Cleanup(func() {
		_ = bank.Delete(context.Background(), wsA, memory.LayerSession, "greeting")
		_ = bank.Delete(context.Background(), wsB, memory.LayerSession, "greeting")
	})

	vA, err := bank.Read(memCtx, wsA, memory.LayerSession, "greeting")
	require.NoError(t, err)
	require.Equal(t, "hello-A", string(vA))

	vB, err := bank.Read(memCtx, wsB, memory.LayerSession, "greeting")
	require.NoError(t, err)
	require.Equal(t, "hello-B", string(vB))

	// Cross-workspace read of a value written under a different workspace is
	// not found (deny-by-default / no leakage).
	require.NoError(t, bank.Write(memCtx, wsA, memory.LayerLongTerm, "draft", []byte("wip"), time.Minute))
	t.Cleanup(func() { _ = bank.Delete(context.Background(), wsA, memory.LayerLongTerm, "draft") })
	_, err = bank.Read(memCtx, wsB, memory.LayerLongTerm, "draft")
	require.True(t, errors.Is(err, memory.ErrNotFound), "workspace B must not read workspace A long-term memory")

	// Audit recorded for the two writes in workspace A.
	require.Equal(t, 3, audit.count("memory.write", "success"), "expected write audits for both workspaces")
}

// TestMemoryBankSecretAndBoundsIntegration verifies abuse controls at the live
// boundary: no secret is ever written to memory and oversize values are refused.
func TestMemoryBankSecretAndBoundsIntegration(t *testing.T) {
	rdb := redisClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store := redis.NewMemoryStore(rdb)
	bank := memory.NewBank(store, nil, nil)
	ws := uuid.MustParse(workspaceA)

	require.ErrorIs(t, bank.Write(ctx, ws, memory.LayerSession, "token", []byte("sk-abcdef"), time.Minute), memory.ErrSecretRejected)

	big := make([]byte, 65537)
	require.ErrorIs(t, bank.Write(ctx, ws, memory.LayerSession, "blob", big, time.Minute), memory.ErrValueTooLarge)

	// An oversized/blank key is refused before it reaches Redis.
	require.ErrorIs(t, bank.Write(ctx, ws, memory.LayerSession, "", []byte("v"), time.Minute), memory.ErrInvalidKey)
}
