package department

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	NameMaxRunes = 100
)

// Service enforces workspace ownership and validation for departments.
type Service struct {
	store Store
}

// NewService creates a department service.
func NewService(s Store) *Service {
	return &Service{store: s}
}

// Create validates and creates a department.
func (svc *Service) Create(ctx context.Context, workspaceID uuid.UUID, name string) (*Department, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidInput
	}
	if utf8.RuneCountInString(name) > NameMaxRunes {
		return nil, ErrInvalidInput
	}
	if workspaceID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	d, err := New(workspaceID, name)
	if err != nil {
		return nil, err
	}
	return svc.store.Create(ctx, d)
}

// Get returns a department by id, scoped to workspace.
func (svc *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Department, error) {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return nil, ErrInvalidInput
	}
	return svc.store.Get(ctx, workspaceID, id)
}

// List returns all departments in a workspace, deterministically ordered.
func (svc *Service) List(ctx context.Context, workspaceID uuid.UUID) ([]*Department, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	return svc.store.List(ctx, workspaceID)
}

// ListPage returns a bounded page.
func (svc *Service) ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error) {
	if workspaceID == uuid.Nil {
		return Page{}, ErrWorkspaceMismatch
	}
	q.Normalize()
	return svc.store.ListPage(ctx, workspaceID, q)
}

// Update renames a department.
func (svc *Service) Update(ctx context.Context, workspaceID, id uuid.UUID, name string) (*Department, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidInput
	}
	if utf8.RuneCountInString(name) > NameMaxRunes {
		return nil, ErrInvalidInput
	}
	existing, err := svc.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	existing.Name = name
	return svc.store.Update(ctx, workspaceID, existing)
}

// Delete removes a department (cascades to teams and employees via FK).
func (svc *Service) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return ErrInvalidInput
	}
	return svc.store.Delete(ctx, workspaceID, id)
}
