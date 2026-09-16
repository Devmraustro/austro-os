package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"austro-os/internal/department"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// DepartmentStore is the Postgres adapter for department.Store.
type DepartmentStore struct {
	db *sql.DB
}

func NewDepartmentStore(db *sql.DB) *DepartmentStore {
	return &DepartmentStore{db: db}
}

func (s *DepartmentStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('app.current_workspace', $1, true)",
		workspaceID.String()); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (s *DepartmentStore) Create(ctx context.Context, d *department.Department) (*department.Department, error) {
	tx, err := s.beginTx(ctx, d.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id uuid.UUID
	var createdAt, updatedAt time.Time
	// Atomic uniqueness guard: insert only if name not exists in workspace.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO departments (id, workspace_id, name, created_at)
		SELECT $1, $2, $3, $4
		WHERE NOT EXISTS (SELECT 1 FROM departments WHERE workspace_id = $2 AND lower(name) = lower($3))
		RETURNING id, created_at, created_at`,
		d.ID, d.WorkspaceID, d.Name, d.CreatedAt).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, department.ErrNameTaken
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, department.ErrNameTaken
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	d.ID = id
	d.CreatedAt = createdAt
	d.UpdatedAt = updatedAt
	return d, nil
}

func (s *DepartmentStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*department.Department, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var d department.Department
	var wsID uuid.UUID
	var name string
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT id, workspace_id, name, created_at FROM departments
		WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&d.ID, &wsID, &name, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, department.ErrNotFound
		}
		return nil, err
	}
	d.WorkspaceID = wsID
	d.Name = name
	d.CreatedAt = createdAt
	d.UpdatedAt = createdAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *DepartmentStore) List(ctx context.Context, workspaceID uuid.UUID) ([]*department.Department, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, workspace_id, name, created_at FROM departments
		WHERE workspace_id = $1 ORDER BY created_at, name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*department.Department
	for rows.Next() {
		var d department.Department
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.UpdatedAt = d.CreatedAt
		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *DepartmentStore) ListPage(ctx context.Context, workspaceID uuid.UUID, q department.ListQuery) (department.Page, error) {
	q.Normalize()
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return department.Page{}, err
	}
	defer tx.Rollback()

	query := `SELECT id, workspace_id, name, created_at FROM departments WHERE workspace_id = $1`
	args := []interface{}{workspaceID}
	if q.Cursor.Set {
		// Use (created_at, id) < (cursor_created, cursor_id) for newest-first? But we order ASC for deterministic.
		// We use created_at DESC, id DESC like tasks.
		// For simplicity, we fetch with ORDER BY created_at DESC, id DESC and cursor is id only.
		// To properly page, we need created_at of cursor. We'll lookup cursor row if needed.
		// Simplified: if cursor set, filter where (created_at, id) < (cursor_time, cursor_id) or > depending on order.
		// We implement ORDER BY created_at DESC, id DESC.
		// So we need to get cursor's created_at.
		var cursorCreated time.Time
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM departments WHERE id = $1 AND workspace_id = $2`, q.Cursor.ID, workspaceID).Scan(&cursorCreated)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return department.Page{}, department.ErrInvalidInput
			}
			return department.Page{}, err
		}
		args = append(args, cursorCreated, q.Cursor.ID)
		query += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, q.Limit+1)
	query += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return department.Page{}, err
	}
	defer rows.Close()

	var out []*department.Department
	for rows.Next() {
		var d department.Department
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.CreatedAt); err != nil {
			return department.Page{}, err
		}
		d.UpdatedAt = d.CreatedAt
		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return department.Page{}, err
	}
	if err := tx.Commit(); err != nil {
		return department.Page{}, err
	}

	page := department.Page{Limit: q.Limit}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		page.NextCursor = department.EncodeCursor(out[len(out)-1])
	}
	page.Departments = out
	return page, nil
}

func (s *DepartmentStore) Update(ctx context.Context, workspaceID uuid.UUID, d *department.Department) (*department.Department, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Check uniqueness if name changed.
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM departments WHERE workspace_id = $1 AND lower(name) = lower($2) AND id != $3)`, workspaceID, d.Name, d.ID).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, department.ErrNameTaken
	}

	var updated department.Department
	err = tx.QueryRowContext(ctx, `
		UPDATE departments SET name = $1 WHERE id = $2 AND workspace_id = $3
		RETURNING id, workspace_id, name, created_at`, d.Name, d.ID, workspaceID).Scan(&updated.ID, &updated.WorkspaceID, &updated.Name, &updated.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, department.ErrNotFound
		}
		return nil, err
	}
	updated.UpdatedAt = updated.CreatedAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &updated, nil
}

func (s *DepartmentStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM departments WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return department.ErrNotFound
	}
	return tx.Commit()
}

// Ensure DepartmentStore implements department.Store
var _ department.Store = (*DepartmentStore)(nil)

// DepartmentResolver adapter for team service to validate department ownership.
type DepartmentResolverAdapter struct {
	store *DepartmentStore
}

func NewDepartmentResolverAdapter(store *DepartmentStore) *DepartmentResolverAdapter {
	return &DepartmentResolverAdapter{store: store}
}

func (a *DepartmentResolverAdapter) Get(ctx context.Context, workspaceID, departmentID uuid.UUID) (uuid.UUID, error) {
	tx, err := a.store.beginTx(ctx, workspaceID)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback()

	var wsID uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT workspace_id FROM departments WHERE id = $1`, departmentID).Scan(&wsID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, department.ErrNotFound
		}
		return uuid.Nil, err
	}
	if wsID != workspaceID {
		return uuid.Nil, department.ErrWorkspaceMismatch
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil, err
	}
	return wsID, nil
}
