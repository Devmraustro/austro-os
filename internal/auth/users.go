package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrUserNotFound reports a user lookup that matched no identity.
var ErrUserNotFound = errors.New("user not found")

// ErrUserExists reports a conflicting identity creation: either the username
// already exists or a founder already exists (the system allows at most one).
var ErrUserExists = errors.New("user already exists")

// UserRecord is the persisted identity a token is minted for. PasswordHash is
// only ever read for credential verification; it is never serialized into
// token claims or API responses.
type UserRecord struct {
	ID           string
	Username     string
	DisplayName  string
	Email        string
	IsFounder    bool
	WorkspaceID  string // empty for an organization-level founder identity
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserStore persists identities. The default implementation is in-memory;
// production uses the PostgreSQL adapter in infrastructure/postgres. Only the
// bcrypt hash of a password is ever stored.
type UserStore interface {
	// ByUsername returns the identity for a username, or ErrUserNotFound.
	ByUsername(ctx context.Context, username string) (*UserRecord, error)
	// ByID returns the identity for an id, or ErrUserNotFound.
	ByID(ctx context.Context, id string) (*UserRecord, error)
	// Create inserts a new identity. A duplicate username or a second founder
	// returns ErrUserExists.
	Create(ctx context.Context, u *UserRecord) (*UserRecord, error)
}

// memoryUserStore is a concurrency-safe in-memory UserStore for tests and
// single-instance deployments, mirroring NewMemoryRefreshStore.
type memoryUserStore struct {
	mu         sync.RWMutex
	byUsername map[string]*UserRecord
	byID       map[string]*UserRecord
}

// NewMemoryUserStore returns an in-memory UserStore.
func NewMemoryUserStore() UserStore {
	return &memoryUserStore{
		byUsername: map[string]*UserRecord{},
		byID:       map[string]*UserRecord{},
	}
}

func (m *memoryUserStore) ByUsername(_ context.Context, username string) (*UserRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.byUsername[username]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneUser(u), nil
}

func (m *memoryUserStore) ByID(_ context.Context, id string) (*UserRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.byID[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneUser(u), nil
}

func (m *memoryUserStore) Create(_ context.Context, u *UserRecord) (*UserRecord, error) {
	if u == nil {
		return nil, errors.New("user is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byUsername[u.Username]; ok {
		return nil, ErrUserExists
	}
	if u.IsFounder {
		for _, existing := range m.byID {
			if existing.IsFounder {
				return nil, ErrUserExists
			}
		}
	}
	c := cloneUser(u)
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	m.byUsername[c.Username] = c
	m.byID[c.ID] = c
	return cloneUser(c), nil
}

func cloneUser(u *UserRecord) *UserRecord {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}
