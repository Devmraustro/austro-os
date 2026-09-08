// Package workspace defines the persistence port for organization-level
// workspace administration.
//
// Workspace scope is enforced upstream by the authorization layer
// (internal/rbac + internal/authz), never by this store. The store exposes only
// the three operations the workspace administration API performs — create, read
// by id, and the founder's organization-level listing. No call here accepts a
// client-supplied workspace context or a cross-workspace filter: a caller can
// only ever act on the workspace its verified claims bind it to, or on
// organization-level data through an explicit founder rule.
package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound reports a workspace id with no matching row.
var ErrNotFound = errors.New("workspace not found")

// ErrNameTaken reports a workspace name that already exists. The store guards
// against duplicates atomically so that a sequential founder caller gets a
// deterministic conflict instead of a silently duplicated workspace.
var ErrNameTaken = errors.New("workspace name already exists")

// Workspace is the persisted organization-level workspace record.
type Workspace struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is the port for workspace administration data.
type Store interface {
	// Create persists a workspace with the given name and returns the record.
	// It returns ErrNameTaken when a workspace with that name already exists.
	Create(ctx context.Context, name string) (*Workspace, error)
	// Get returns the workspace with the given id, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (*Workspace, error)
	// List returns every workspace, deterministically ordered.
	List(ctx context.Context) ([]*Workspace, error)
}
