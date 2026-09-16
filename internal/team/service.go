package team

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const NameMaxRunes = 100

// DepartmentResolver resolves a department's workspace for cross-workspace validation.
type DepartmentResolver interface {
	Get(ctx context.Context, workspaceID, departmentID uuid.UUID) (workspaceIDOfDept uuid.UUID, err error)
}

// Service enforces workspace ownership and hierarchy invariants for teams.
type Service struct {
	store            Store
	departmentLookup DepartmentResolver
}

// NewService creates a team service.
func NewService(s Store, resolver DepartmentResolver) *Service {
	return &Service{store: s, departmentLookup: resolver}
}

// Create validates and creates a team, ensuring department belongs to same workspace.
func (svc *Service) Create(ctx context.Context, workspaceID, departmentID uuid.UUID, name string) (*Team, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidInput
	}
	if utf8.RuneCountInString(name) > NameMaxRunes {
		return nil, ErrInvalidInput
	}
	if workspaceID == uuid.Nil || departmentID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	// Verify department exists in this workspace (prevents cross-workspace parent).
	if svc.departmentLookup != nil {
		_, err := svc.departmentLookup.Get(ctx, workspaceID, departmentID)
		if err != nil {
			return nil, err
		}
	}
	t, err := New(departmentID, workspaceID, name)
	if err != nil {
		return nil, err
	}
	return svc.store.Create(ctx, t)
}

func (svc *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Team, error) {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return nil, ErrInvalidInput
	}
	return svc.store.Get(ctx, workspaceID, id)
}

func (svc *Service) List(ctx context.Context, workspaceID uuid.UUID, departmentID *uuid.UUID) ([]*Team, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	return svc.store.List(ctx, workspaceID, departmentID)
}

func (svc *Service) ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error) {
	if workspaceID == uuid.Nil {
		return Page{}, ErrWorkspaceMismatch
	}
	q.Normalize()
	return svc.store.ListPage(ctx, workspaceID, q)
}

// Update renames or moves a team to another department within same workspace.
func (svc *Service) Update(ctx context.Context, workspaceID, id uuid.UUID, name string, departmentID *uuid.UUID) (*Team, error) {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return nil, ErrInvalidInput
	}
	existing, err := svc.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if name != "" {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return nil, ErrInvalidInput
		}
		if utf8.RuneCountInString(trimmed) > NameMaxRunes {
			return nil, ErrInvalidInput
		}
		existing.Name = trimmed
	}
	if departmentID != nil {
		if *departmentID == uuid.Nil {
			return nil, ErrInvalidInput
		}
		if svc.departmentLookup != nil {
			_, err := svc.departmentLookup.Get(ctx, workspaceID, *departmentID)
			if err != nil {
				return nil, err
			}
		}
		existing.DepartmentID = *departmentID
	}
	return svc.store.Update(ctx, workspaceID, existing)
}

func (svc *Service) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return ErrInvalidInput
	}
	return svc.store.Delete(ctx, workspaceID, id)
}
