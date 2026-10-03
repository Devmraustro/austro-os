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
// administrative handle rather than the runtime one: the runtime role is not
// the table owner and row level security is forced, so an unbound runtime
// session sees no workspace at all, which would make organization-level
// listing and creation impossible. The administrative role's reach over this
// table is exactly org_admin_policy -- it is not a superuser and has no
// BYPASSRLS. Authorization for who may list/create/read is enforced upstream
// by the RBAC layer, never here.
type WorkspaceStore struct {
	db *sql.DB
}

// NewWorkspaceStore returns a Postgres-backed implementation of
// workspace.Store.
func NewWorkspaceStore(db *sql.DB) *WorkspaceStore {
	return &WorkspaceStore{db: db}
}

// Create inserts a workspace and returns the persisted record. Uniqueness of
// the name is enforced by the uq_workspaces_name constraint rather than by a
// read before the write: a `SELECT ... WHERE NOT EXISTS` guard reads a snapshot
// and then writes outside it, so two creates racing each other can both observe
// the name as free and both insert. The constraint makes the decision inside the
// write, so of two concurrent creates exactly one succeeds and the other is
// rejected by the database, which surfaces here as ErrNameTaken and at the API
// as the documented 409.
func (s *WorkspaceStore) Create(ctx context.Context, name string) (*workspace.Workspace, error) {
	var w workspace.Workspace
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO workspaces (name)
		VALUES ($1)
		RETURNING id, name, created_at, updated_at`, name).
		Scan(&w.ID, &w.Name, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
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
