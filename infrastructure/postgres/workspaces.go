package postgres

import (
	"context"
	"database/sql"
	"errors"

	"austro-os/internal/workspace"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// WorkspaceStore is the concrete PostgreSQL adapter for the workspace.Store
// port. Workspaces are organization-level records, so operations run on the
// plain connection (no app.current_workspace binding): the RLS policy on the
// workspaces table only ever exposes a workspace to a restricted principal
// whose session is bound to that exact workspace id, and the API process itself
// runs as the table owner. Authorization for who may list/create/read is
// enforced upstream by the RBAC layer, never here.
type WorkspaceStore struct {
	db *sql.DB
}

// NewWorkspaceStore returns a Postgres-backed implementation of
// workspace.Store.
func NewWorkspaceStore(db *sql.DB) *WorkspaceStore {
	return &WorkspaceStore{db: db}
}

// Create inserts a workspace and returns the persisted record. The insert is
// guarded against an existing name atomically, so a duplicate never silently
// creates a second workspace and a sequential caller always receives a
// deterministic ErrNameTaken.
func (s *WorkspaceStore) Create(ctx context.Context, name string) (*workspace.Workspace, error) {
	var w workspace.Workspace
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO workspaces (name)
		SELECT $1
		WHERE NOT EXISTS (SELECT 1 FROM workspaces WHERE name = $1)
		RETURNING id, name, created_at, updated_at`, name).
		Scan(&w.ID, &w.Name, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Defense-in-depth: a unique constraint anywhere on name surfaces
			// as a conflict, not as an internal error.
			return nil, workspace.ErrNameTaken
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, workspace.ErrNameTaken
		}
		return nil, err
	}
	return &w, nil
}

// Get returns the workspace with the given id, or workspace.ErrNotFound. Reads
// are id-scoped by the WHERE clause; callers are already authorized to read that
// id by the RBAC layer (founder org-level, or workspace admin bound to the id
// in its verified claims).
func (s *WorkspaceStore) Get(ctx context.Context, id uuid.UUID) (*workspace.Workspace, error) {
	var w workspace.Workspace
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, created_at, updated_at FROM workspaces WHERE id = $1`, id).
		Scan(&w.ID, &w.Name, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, workspace.ErrNotFound
		}
		return nil, err
	}
	return &w, nil
}

// List returns every workspace, deterministically ordered. Only the founder's
// organization-level rule reaches this method; restricted principals are
// denied before any store call and, at the SQL boundary, the workspaces RLS
// policy still filters what they could ever see.
func (s *WorkspaceStore) List(ctx context.Context) ([]*workspace.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, created_at, updated_at FROM workspaces ORDER BY created_at, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*workspace.Workspace
	for rows.Next() {
		var w workspace.Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
