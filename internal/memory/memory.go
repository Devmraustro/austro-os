package memory

import (
	"context"
	"fmt"
	"time"

	"austro-os/internal/config"
	"austro-os/internal/repository"
)

type MemoryLayer int

const (
	LayerSession MemoryLayer = iota
	LayerLongTerm
	LayerWorkspace
	LayerOrganizational
)

type MemoryStore interface {
	Store(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Retrieve(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	ListPrefix(ctx context.Context, prefix string) ([]string, error)
}

type sessionMemory struct {
	repo repository.Repository
	ctx  context.Context
}

func NewSession(repo repository.Repository, ctx context.Context) *sessionMemory {
	return &sessionMemory{repo: repo, ctx: ctx}
}

func (m *sessionMemory) Store(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return m.repo.StoreSessionMemory(ctx, key, value, ttl)
}

func (m *sessionMemory) Retrieve(ctx context.Context, key string) ([]byte, error) {
	return m.repo.RetrieveSessionMemory(ctx, key)
}

func (m *sessionMemory) Delete(ctx context.Context, key string) error {
	return m.repo.DeleteSessionMemory(ctx, key)
}

func (m *sessionMemory) ListPrefix(ctx context.Context, prefix string) ([]string, error) {
	return m.repo.ListSessionMemoryPrefix(ctx, prefix)
}

type longTermMemory struct {
	repo repository.Repository
	ctx  context.Context
}

func NewLongTerm(repo repository.Repository, ctx context.Context) *longTermMemory {
	return &longTermMemory{repo: repo, ctx: ctx}
}

func (m *longTermMemory) Store(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return m.repo.StoreLongTermMemory(ctx, key, value, ttl)
}

func (m *longTermMemory) Retrieve(ctx context.Context, key string) ([]byte, error) {
	return m.repo.RetrieveLongTermMemory(ctx, key)
}

func (m *longTermMemory) Delete(ctx context.Context, key string) error {
	return m.repo.DeleteLongTermMemory(ctx, key)
}

func (m *longTermMemory) ListPrefix(ctx context.Context, prefix string) ([]string, error) {
	return m.repo.ListLongTermMemoryPrefix(ctx, prefix)
}

type workspaceMemory struct {
	repo repository.Repository
	ctx  context.Context
}

func NewWorkspace(repo repository.Repository, ctx context.Context) *workspaceMemory {
	return &workspaceMemory{repo: repo, ctx: ctx}
}

func (m *workspaceMemory) Store(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	partitioned := fmt.Sprintf("workspace:%s:%s", config.Get().WorkspaceID, key)
	return m.repo.StoreWorkspaceMemory(ctx, partitioned, value, ttl)
}

func (m *workspaceMemory) Retrieve(ctx context.Context, key string) ([]byte, error) {
	partitioned := fmt.Sprintf("workspace:%s:%s", config.Get().WorkspaceID, key)
	return m.repo.RetrieveWorkspaceMemory(ctx, partitioned)
}

func (m *workspaceMemory) Delete(ctx context.Context, key string) error {
	partitioned := fmt.Sprintf("workspace:%s:%s", config.Get().WorkspaceID, key)
	return m.repo.DeleteWorkspaceMemory(ctx, partitioned)
}

func (m *workspaceMemory) ListPrefix(ctx context.Context, prefix string) ([]string, error) {
	partitioned := fmt.Sprintf("workspace:%s:%s", config.Get().WorkspaceID, prefix)
	return m.repo.ListWorkspaceMemoryPrefix(ctx, partitioned)
}

type organizationalMemory struct {
	repo repository.Repository
	ctx  context.Context
}

func NewOrganizational(repo repository.Repository, ctx context.Context) *organizationalMemory {
	return &organizationalMemory{repo: repo, ctx: ctx}
}

func (m *organizationalMemory) Store(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	partitioned := fmt.Sprintf("org:%s", key)
	return m.repo.StoreOrganizationalMemory(ctx, partitioned, value, ttl)
}

func (m *organizationalMemory) Retrieve(ctx context.Context, key string) ([]byte, error) {
	partitioned := fmt.Sprintf("org:%s", key)
	return m.repo.RetrieveOrganizationalMemory(ctx, partitioned)
}

func (m *organizationalMemory) Delete(ctx context.Context, key string) error {
	partitioned := fmt.Sprintf("org:%s", key)
	return m.repo.DeleteOrganizationalMemory(ctx, partitioned)
}

func (m *organizationalMemory) ListPrefix(ctx context.Context, prefix string) ([]string, error) {
	partitioned := fmt.Sprintf("org:%s", prefix)
	return m.repo.ListOrganizationalMemoryPrefix(ctx, partitioned)
}