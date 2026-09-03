package redis

import (
	"context"
	"errors"
	"time"

	"austro-os/internal/memory"

	"github.com/redis/go-redis/v9"
)

// MemoryStore is the concrete Redis-backed adapter for the memory persistence
// port. Keys are already namespace-partitioned by the caller (e.g.
// workspace:<id>:session:<key> or org:<key>), so the adapter stores each cell
// verbatim with its TTL. A missing value is reported with memory.ErrNotFound so
// callers distinguish absence from transport errors.

// NewMemoryStore returns a MemoryStore bound to an existing Redis client. The
// caller owns the client lifecycle (see Initialize).
func NewMemoryStore(client *redis.Client) *MemoryStore {
	return &MemoryStore{client: client}
}

// MemoryStore implements the four-layer memory persistence over a single Redis
// keyspace. Intended to satisfy the internal memory facade's persistence port.
type MemoryStore struct {
	client *redis.Client
}

func (m *MemoryStore) store(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return m.client.Set(ctx, key, value, ttl).Err()
}

func (m *MemoryStore) retrieve(ctx context.Context, key string) ([]byte, error) {
	val, err := m.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, memory.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return val, nil
}

func (m *MemoryStore) del(ctx context.Context, key string) error {
	if err := m.client.Del(ctx, key).Err(); err != nil {
		return err
	}
	return nil
}

// Session layer.
func (m *MemoryStore) StoreSessionMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return m.store(ctx, key, value, ttl)
}
func (m *MemoryStore) RetrieveSessionMemory(ctx context.Context, key string) ([]byte, error) {
	return m.retrieve(ctx, key)
}
func (m *MemoryStore) DeleteSessionMemory(ctx context.Context, key string) error {
	return m.del(ctx, key)
}

// Long-term layer.
func (m *MemoryStore) StoreLongTermMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return m.store(ctx, key, value, ttl)
}
func (m *MemoryStore) RetrieveLongTermMemory(ctx context.Context, key string) ([]byte, error) {
	return m.retrieve(ctx, key)
}
func (m *MemoryStore) DeleteLongTermMemory(ctx context.Context, key string) error {
	return m.del(ctx, key)
}

// Workspace layer (partitioned by caller).
func (m *MemoryStore) StoreWorkspaceMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return m.store(ctx, key, value, ttl)
}
func (m *MemoryStore) RetrieveWorkspaceMemory(ctx context.Context, key string) ([]byte, error) {
	return m.retrieve(ctx, key)
}
func (m *MemoryStore) DeleteWorkspaceMemory(ctx context.Context, key string) error {
	return m.del(ctx, key)
}

// Organizational layer (org-wide).
func (m *MemoryStore) StoreOrganizationalMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return m.store(ctx, key, value, ttl)
}
func (m *MemoryStore) RetrieveOrganizationalMemory(ctx context.Context, key string) ([]byte, error) {
	return m.retrieve(ctx, key)
}
func (m *MemoryStore) DeleteOrganizationalMemory(ctx context.Context, key string) error {
	return m.del(ctx, key)
}
