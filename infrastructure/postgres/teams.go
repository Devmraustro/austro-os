package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"austro-os/internal/team"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type TeamStore struct {
	db *sql.DB
}

func NewTeamStore(db *sql.DB) *TeamStore {
	return &TeamStore{db: db}
}

func (s *TeamStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
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

func (s *TeamStore) Create(ctx context.Context, t *team.Team) (*team.Team, error) {
	tx, err := s.beginTx(ctx, t.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Verify department belongs to workspace (defense-in-depth, RLS also enforces).
	var deptWS uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT workspace_id FROM departments WHERE id = $1`, t.DepartmentID).Scan(&deptWS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, team.ErrInvalidInput
		}
		return nil, err
	}
	if deptWS != t.WorkspaceID {
		return nil, team.ErrWorkspaceMismatch
	}

	var id uuid.UUID
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO teams (id, department_id, name, created_at)
		SELECT $1, $2, $3, $4
		WHERE NOT EXISTS (SELECT 1 FROM teams WHERE department_id = $2 AND lower(name) = lower($3))
		RETURNING id, created_at`,
		t.ID, t.DepartmentID, t.Name, t.CreatedAt).Scan(&id, &createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, team.ErrNameTaken
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, team.ErrNameTaken
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	t.ID = id
	t.CreatedAt = createdAt
	t.UpdatedAt = createdAt
	return t, nil
}

func (s *TeamStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*team.Team, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var t team.Team
	var deptID uuid.UUID
	var name string
	var createdAt time.Time
	// Join to verify workspace ownership via department.
	err = tx.QueryRowContext(ctx, `
		SELECT t.id, t.department_id, t.name, t.created_at, d.workspace_id
		FROM teams t
		JOIN departments d ON t.department_id = d.id
		WHERE t.id = $1 AND d.workspace_id = $2`, id, workspaceID).Scan(&t.ID, &deptID, &name, &createdAt, &t.WorkspaceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, team.ErrNotFound
		}
		return nil, err
	}
	t.DepartmentID = deptID
	t.Name = name
	t.CreatedAt = createdAt
	t.UpdatedAt = createdAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *TeamStore) List(ctx context.Context, workspaceID uuid.UUID, departmentID *uuid.UUID) ([]*team.Team, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	query := `
		SELECT t.id, t.department_id, t.name, t.created_at, d.workspace_id
		FROM teams t
		JOIN departments d ON t.department_id = d.id
		WHERE d.workspace_id = $1`
	args := []interface{}{workspaceID}
	if departmentID != nil {
		query += " AND t.department_id = $2"
		args = append(args, *departmentID)
	}
	query += " ORDER BY t.created_at, t.name"

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*team.Team
	for rows.Next() {
		var tm team.Team
		if err := rows.Scan(&tm.ID, &tm.DepartmentID, &tm.Name, &tm.CreatedAt, &tm.WorkspaceID); err != nil {
			return nil, err
		}
		tm.UpdatedAt = tm.CreatedAt
		out = append(out, &tm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *TeamStore) ListPage(ctx context.Context, workspaceID uuid.UUID, q team.ListQuery) (team.Page, error) {
	q.Normalize()
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return team.Page{}, err
	}
	defer tx.Rollback()

	query := `
		SELECT t.id, t.department_id, t.name, t.created_at, d.workspace_id
		FROM teams t
		JOIN departments d ON t.department_id = d.id
		WHERE d.workspace_id = $1`
	args := []interface{}{workspaceID}
	if q.DepartmentID != nil {
		query += fmt.Sprintf(" AND t.department_id = $%d", len(args)+1)
		args = append(args, *q.DepartmentID)
	}
	if q.Cursor.Set {
		var cursorCreated time.Time
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM teams WHERE id = $1`, q.Cursor.ID).Scan(&cursorCreated)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return team.Page{}, team.ErrInvalidInput
			}
			return team.Page{}, err
		}
		args = append(args, cursorCreated, q.Cursor.ID)
		query += fmt.Sprintf(" AND (t.created_at, t.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, q.Limit+1)
	query += fmt.Sprintf(" ORDER BY t.created_at DESC, t.id DESC LIMIT $%d", len(args))

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return team.Page{}, err
	}
	defer rows.Close()

	var out []*team.Team
	for rows.Next() {
		var tm team.Team
		if err := rows.Scan(&tm.ID, &tm.DepartmentID, &tm.Name, &tm.CreatedAt, &tm.WorkspaceID); err != nil {
			return team.Page{}, err
		}
		tm.UpdatedAt = tm.CreatedAt
		out = append(out, &tm)
	}
	if err := rows.Err(); err != nil {
		return team.Page{}, err
	}
	if err := tx.Commit(); err != nil {
		return team.Page{}, err
	}

	page := team.Page{Limit: q.Limit}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		page.NextCursor = team.EncodeCursor(out[len(out)-1])
	}
	page.Teams = out
	return page, nil
}

func (s *TeamStore) Update(ctx context.Context, workspaceID uuid.UUID, t *team.Team) (*team.Team, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Verify new department belongs to same workspace if changed.
	var deptWS uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT workspace_id FROM departments WHERE id = $1`, t.DepartmentID).Scan(&deptWS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, team.ErrInvalidInput
		}
		return nil, err
	}
	if deptWS != workspaceID {
		return nil, team.ErrWorkspaceMismatch
	}

	// Check name uniqueness within department.
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM teams WHERE department_id = $1 AND lower(name) = lower($2) AND id != $3)`, t.DepartmentID, t.Name, t.ID).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, team.ErrNameTaken
	}

	var updated team.Team
	var name string
	var deptID uuid.UUID
	var createdAt time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE teams SET department_id = $1, name = $2 WHERE id = $3
		RETURNING id, department_id, name, created_at`, t.DepartmentID, t.Name, t.ID).Scan(&updated.ID, &deptID, &name, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, team.ErrNotFound
		}
		return nil, err
	}
	updated.DepartmentID = deptID
	updated.Name = name
	updated.CreatedAt = createdAt
	updated.UpdatedAt = createdAt
	updated.WorkspaceID = workspaceID
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &updated, nil
}

func (s *TeamStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Verify ownership via join.
	var exists bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM teams t JOIN departments d ON t.department_id = d.id WHERE t.id = $1 AND d.workspace_id = $2)`, id, workspaceID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return team.ErrNotFound
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM teams WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

var _ team.Store = (*TeamStore)(nil)

// TeamResolverAdapter for aiemployee service.
type TeamResolverAdapter struct {
	store *TeamStore
}

func NewTeamResolverAdapter(store *TeamStore) *TeamResolverAdapter {
	return &TeamResolverAdapter{store: store}
}

func (a *TeamResolverAdapter) Get(ctx context.Context, workspaceID, teamID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	tx, err := a.store.beginTx(ctx, workspaceID)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	defer tx.Rollback()

	var wsID, deptID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT d.workspace_id, t.department_id FROM teams t
		JOIN departments d ON t.department_id = d.id
		WHERE t.id = $1`, teamID).Scan(&wsID, &deptID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, uuid.Nil, team.ErrNotFound
		}
		return uuid.Nil, uuid.Nil, err
	}
	if wsID != workspaceID {
		return uuid.Nil, uuid.Nil, team.ErrWorkspaceMismatch
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return wsID, deptID, nil
}
