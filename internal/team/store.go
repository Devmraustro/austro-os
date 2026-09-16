package team

import (
	"context"

	"github.com/google/uuid"
)

// Store is the persistence port for teams.
type Store interface {
	Create(ctx context.Context, t *Team) (*Team, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Team, error)
	List(ctx context.Context, workspaceID uuid.UUID, departmentID *uuid.UUID) ([]*Team, error)
	ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error)
	Update(ctx context.Context, workspaceID uuid.UUID, t *Team) (*Team, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}
