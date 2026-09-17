// Package department store port.
package department

import (
	"context"

	"github.com/google/uuid"
)

// Store is the persistence port for departments.
type Store interface {
	Create(ctx context.Context, d *Department) (*Department, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Department, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]*Department, error)
	ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error)
	Update(ctx context.Context, workspaceID uuid.UUID, d *Department) (*Department, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}
