package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"austro-os/internal/task"

	"github.com/google/uuid"
)

// TaskStore is the concrete Postgres adapter for the task.TaskStore port. Each
// operation runs inside a transaction that first binds app.current_workspace on
// that exact connection (transaction-local scope), so row-level security
// genuinely constrains the operation. Explicit workspace_id guards are kept as
// defense-in-depth for callers whose connection may bypass RLS.
type TaskStore struct {
	db *sql.DB
}

// NewTaskStore returns a Postgres-backed implementation of task.TaskStore.
func NewTaskStore(db *sql.DB) *TaskStore {
	return &TaskStore{db: db}
}

// beginTx opens an exclusive transaction and binds the workspace context on it
// so RLS resolves correctly for the whole operation.
func (s *TaskStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
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

func (s *TaskStore) Create(ctx context.Context, t *task.Task) (*task.Task, error) {
	tx, err := s.beginTx(ctx, t.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	assigneeType := string(t.AssigneeType)
	if assigneeType == "" {
		assigneeType = string(task.AssigneeAI)
	}
	var id uuid.UUID
	var createdAt, updatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO tasks (workspace_id, title, description, status, priority,
		                   assignee_type, assignee_id, deadline, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, created_at, updated_at`,
		t.WorkspaceID, t.Title, nullableString(t.Description), string(t.Status),
		string(t.Priority), assigneeType, nullableUUID(t.AssigneeID),
		nullableTime(t.Deadline), t.CreatedAt, t.UpdatedAt).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	t.ID = id
	t.AssigneeType = task.AssigneeType(assigneeType)
	t.CreatedAt = createdAt
	t.UpdatedAt = updatedAt
	return t, nil
}

func (s *TaskStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*task.Task, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
		SELECT id, workspace_id, title, description, status, priority,
		       assignee_type, assignee_id, deadline, created_at, updated_at
		FROM tasks WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	t, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *TaskStore) List(ctx context.Context, workspaceID uuid.UUID, status *task.Status) ([]*task.Task, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	query := `
		SELECT id, workspace_id, title, description, status, priority,
		       assignee_type, assignee_id, deadline, created_at, updated_at
		FROM tasks WHERE workspace_id = $1`
	args := []interface{}{workspaceID}
	if status != nil {
		query += " AND status = $2"
		args = append(args, string(*status))
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*task.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *TaskStore) Update(ctx context.Context, workspaceID uuid.UUID, t *task.Task) (*task.Task, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	t.UpdatedAt = time.Now().UTC()
	row := tx.QueryRowContext(ctx, `
		UPDATE tasks SET
			title = $1, description = $2, status = $3, priority = $4,
			assignee_type = $5, assignee_id = $6, deadline = $7, updated_at = $8
		WHERE id = $9 AND workspace_id = $10
		RETURNING id, workspace_id, title, description, status, priority,
		          assignee_type, assignee_id, deadline, created_at, updated_at`,
		t.Title, nullableString(t.Description), string(t.Status), string(t.Priority),
		string(t.AssigneeType), nullableUUID(t.AssigneeID), nullableTime(t.Deadline),
		t.UpdatedAt, t.ID, workspaceID)
	updated, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *TaskStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE id = $1 AND workspace_id = $2", id, workspaceID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return task.ErrNotFound
	}
	return tx.Commit()
}

// rowScanner abstracts the row scanning between *sql.Row and *sql.Rows for a
// single task scan.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanTask(r rowScanner) (*task.Task, error) {
	var t task.Task
	var id, wsID uuid.UUID
	var title string
	var desc sql.NullString
	var status, priority, assigneeType string
	var assigneeID uuid.NullUUID
	var deadline sql.NullTime
	var createdAt, updatedAt time.Time
	if err := r.Scan(&id, &wsID, &title, &desc, &status, &priority,
		&assigneeType, &assigneeID, &deadline, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, task.ErrNotFound
		}
		return nil, err
	}
	t.ID = id
	t.WorkspaceID = wsID
	t.Title = title
	t.Description = desc.String
	t.Status = task.Status(status)
	t.Priority = task.Priority(priority)
	t.AssigneeType = task.AssigneeType(assigneeType)
	if assigneeID.Valid {
		t.AssigneeID = &assigneeID.UUID
	}
	if deadline.Valid {
		t.Deadline = &deadline.Time
	}
	t.CreatedAt = createdAt
	t.UpdatedAt = updatedAt
	return &t, nil
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullableUUID(u *uuid.UUID) interface{} {
	if u == nil {
		return nil
	}
	return *u
}

func nullableTime(u *time.Time) interface{} {
	if u == nil {
		return nil
	}
	return *u
}