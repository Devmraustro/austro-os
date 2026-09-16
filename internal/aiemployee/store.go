package aiemployee

import (
	"context"

	"github.com/google/uuid"
)

// Store is the persistence port for AI employees.
type Store interface {
	Create(ctx context.Context, e *AIEmployee) (*AIEmployee, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*AIEmployee, error)
	List(ctx context.Context, workspaceID uuid.UUID, teamID *uuid.UUID) ([]*AIEmployee, error)
	ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error)
	Update(ctx context.Context, workspaceID uuid.UUID, e *AIEmployee) (*AIEmployee, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}
