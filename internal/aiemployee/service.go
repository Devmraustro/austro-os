package aiemployee

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	NameMaxRunes = 100
	RoleMaxRunes = 100
)

// TeamResolver resolves a team's workspace and department for cross-workspace validation.
type TeamResolver interface {
	Get(ctx context.Context, workspaceID, teamID uuid.UUID) (workspaceIDOfTeam uuid.UUID, departmentID uuid.UUID, err error)
}

// Service enforces workspace ownership and hierarchy invariants.
type Service struct {
	store      Store
	teamLookup TeamResolver
}

// NewService creates an AI employee service.
func NewService(s Store, resolver TeamResolver) *Service {
	return &Service{store: s, teamLookup: resolver}
}

func (svc *Service) Create(ctx context.Context, workspaceID, teamID uuid.UUID, name, role string, capabilities []string) (*AIEmployee, error) {
	name = strings.TrimSpace(name)
	role = strings.TrimSpace(role)
	if name == "" || role == "" {
		return nil, ErrInvalidInput
	}
	if utf8.RuneCountInString(name) > NameMaxRunes || utf8.RuneCountInString(role) > RoleMaxRunes {
		return nil, ErrInvalidInput
	}
	if workspaceID == uuid.Nil || teamID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	var deptID uuid.UUID
	if svc.teamLookup != nil {
		ws, dID, err := svc.teamLookup.Get(ctx, workspaceID, teamID)
		if err != nil {
			return nil, err
		}
		_ = ws
		deptID = dID
	}
	emp, err := New(teamID, workspaceID, deptID, name, role)
	if err != nil {
		return nil, err
	}
	emp.Capabilities = capabilities
	if emp.Capabilities == nil {
		emp.Capabilities = []string{}
	}
	return svc.store.Create(ctx, emp)
}

func (svc *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*AIEmployee, error) {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return nil, ErrInvalidInput
	}
	return svc.store.Get(ctx, workspaceID, id)
}

func (svc *Service) List(ctx context.Context, workspaceID uuid.UUID, teamID *uuid.UUID) ([]*AIEmployee, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	return svc.store.List(ctx, workspaceID, teamID)
}

func (svc *Service) ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error) {
	if workspaceID == uuid.Nil {
		return Page{}, ErrWorkspaceMismatch
	}
	q.Normalize()
	return svc.store.ListPage(ctx, workspaceID, q)
}

// Update allows renaming, changing role, capabilities, and moving to another team within same workspace.
func (svc *Service) Update(ctx context.Context, workspaceID, id uuid.UUID, name, role *string, capabilities []string, teamID *uuid.UUID, currentTaskID *uuid.UUID) (*AIEmployee, error) {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return nil, ErrInvalidInput
	}
	existing, err := svc.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" {
			return nil, ErrInvalidInput
		}
		if utf8.RuneCountInString(trimmed) > NameMaxRunes {
			return nil, ErrInvalidInput
		}
		existing.Name = trimmed
	}
	if role != nil {
		trimmed := strings.TrimSpace(*role)
		if trimmed == "" {
			return nil, ErrInvalidInput
		}
		if utf8.RuneCountInString(trimmed) > RoleMaxRunes {
			return nil, ErrInvalidInput
		}
		existing.Role = trimmed
	}
	if capabilities != nil {
		existing.Capabilities = capabilities
	}
	if teamID != nil {
		if *teamID == uuid.Nil {
			return nil, ErrInvalidInput
		}
		if svc.teamLookup != nil {
			_, deptID, err := svc.teamLookup.Get(ctx, workspaceID, *teamID)
			if err != nil {
				return nil, err
			}
			existing.DepartmentID = deptID
		}
		existing.TeamID = *teamID
	}
	if currentTaskID != nil {
		if *currentTaskID == uuid.Nil {
			existing.CurrentTaskID = nil
		} else {
			existing.CurrentTaskID = currentTaskID
		}
	}
	return svc.store.Update(ctx, workspaceID, existing)
}

func (svc *Service) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return ErrInvalidInput
	}
	return svc.store.Delete(ctx, workspaceID, id)
}
