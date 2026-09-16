package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"austro-os/internal/aiemployee"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
)

type AIEmployeeStore struct {
	db *sql.DB
}

func NewAIEmployeeStore(db *sql.DB) *AIEmployeeStore {
	return &AIEmployeeStore{db: db}
}

func (s *AIEmployeeStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
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

func (s *AIEmployeeStore) Create(ctx context.Context, e *aiemployee.AIEmployee) (*aiemployee.AIEmployee, error) {
	tx, err := s.beginTx(ctx, e.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Verify team belongs to workspace via departments.
	var wsID, deptID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT d.workspace_id, t.department_id FROM teams t
		JOIN departments d ON t.department_id = d.id
		WHERE t.id = $1`, e.TeamID).Scan(&wsID, &deptID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, aiemployee.ErrInvalidInput
		}
		return nil, err
	}
	if wsID != e.WorkspaceID {
		return nil, aiemployee.ErrWorkspaceMismatch
	}
	e.DepartmentID = deptID

	var id uuid.UUID
	var createdAt, updatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO ai_employees (id, team_id, name, role, capabilities, permissions, knowledge_access, current_task_id, created_at, updated_at)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
		WHERE NOT EXISTS (SELECT 1 FROM ai_employees WHERE team_id = $2 AND lower(name) = lower($3))
		RETURNING id, created_at, updated_at`,
		e.ID, e.TeamID, e.Name, e.Role,
		pq.Array(e.Capabilities),
		jsonValue(e.Permissions),
		jsonValue(e.KnowledgeAccess),
		nullableUUID(e.CurrentTaskID),
		e.CreatedAt, e.UpdatedAt).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, aiemployee.ErrNameTaken
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, aiemployee.ErrNameTaken
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	e.ID = id
	e.CreatedAt = createdAt
	e.UpdatedAt = updatedAt
	return e, nil
}

func (s *AIEmployeeStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*aiemployee.AIEmployee, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var e aiemployee.AIEmployee
	var teamID, deptID uuid.UUID
	var name, role string
	var caps pq.StringArray
	var perms, know json.RawMessage
	var memID uuid.NullUUID
	var currTask uuid.NullUUID
	var createdAt, updatedAt time.Time
	var wsID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT ae.id, ae.team_id, t.department_id, d.workspace_id, ae.name, ae.role, ae.capabilities, ae.permissions, ae.knowledge_access, ae.memory_id, ae.current_task_id, ae.created_at, ae.updated_at
		FROM ai_employees ae
		JOIN teams t ON ae.team_id = t.id
		JOIN departments d ON t.department_id = d.id
		WHERE ae.id = $1 AND d.workspace_id = $2`, id, workspaceID).Scan(
		&e.ID, &teamID, &deptID, &wsID, &name, &role, &caps, &perms, &know, &memID, &currTask, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, aiemployee.ErrNotFound
		}
		return nil, err
	}
	e.TeamID = teamID
	e.DepartmentID = deptID
	e.WorkspaceID = wsID
	e.Name = name
	e.Role = role
	e.Capabilities = []string(caps)
	if len(perms) > 0 {
		var p any
		_ = json.Unmarshal(perms, &p)
		e.Permissions = p
	}
	if len(know) > 0 {
		var k any
		_ = json.Unmarshal(know, &k)
		e.KnowledgeAccess = k
	}
	if memID.Valid {
		e.MemoryID = &memID.UUID
	}
	if currTask.Valid {
		e.CurrentTaskID = &currTask.UUID
	}
	e.CreatedAt = createdAt
	e.UpdatedAt = updatedAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *AIEmployeeStore) List(ctx context.Context, workspaceID uuid.UUID, teamID *uuid.UUID) ([]*aiemployee.AIEmployee, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	query := `
		SELECT ae.id, ae.team_id, t.department_id, d.workspace_id, ae.name, ae.role, ae.capabilities, ae.permissions, ae.knowledge_access, ae.memory_id, ae.current_task_id, ae.created_at, ae.updated_at
		FROM ai_employees ae
		JOIN teams t ON ae.team_id = t.id
		JOIN departments d ON t.department_id = d.id
		WHERE d.workspace_id = $1`
	args := []interface{}{workspaceID}
	if teamID != nil {
		query += " AND ae.team_id = $2"
		args = append(args, *teamID)
	}
	query += " ORDER BY ae.created_at, ae.name"

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*aiemployee.AIEmployee
	for rows.Next() {
		var e aiemployee.AIEmployee
		var tID, dID, wID uuid.UUID
		var name, role string
		var caps pq.StringArray
		var perms, know json.RawMessage
		var memID uuid.NullUUID
		var currTask uuid.NullUUID
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&e.ID, &tID, &dID, &wID, &name, &role, &caps, &perms, &know, &memID, &currTask, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		e.TeamID = tID
		e.DepartmentID = dID
		e.WorkspaceID = wID
		e.Name = name
		e.Role = role
		e.Capabilities = []string(caps)
		if len(perms) > 0 {
			var p any
			_ = json.Unmarshal(perms, &p)
			e.Permissions = p
		}
		if len(know) > 0 {
			var k any
			_ = json.Unmarshal(know, &k)
			e.KnowledgeAccess = k
		}
		if memID.Valid {
			e.MemoryID = &memID.UUID
		}
		if currTask.Valid {
			e.CurrentTaskID = &currTask.UUID
		}
		e.CreatedAt = createdAt
		e.UpdatedAt = updatedAt
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *AIEmployeeStore) ListPage(ctx context.Context, workspaceID uuid.UUID, q aiemployee.ListQuery) (aiemployee.Page, error) {
	q.Normalize()
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return aiemployee.Page{}, err
	}
	defer tx.Rollback()

	query := `
		SELECT ae.id, ae.team_id, t.department_id, d.workspace_id, ae.name, ae.role, ae.capabilities, ae.permissions, ae.knowledge_access, ae.memory_id, ae.current_task_id, ae.created_at, ae.updated_at
		FROM ai_employees ae
		JOIN teams t ON ae.team_id = t.id
		JOIN departments d ON t.department_id = d.id
		WHERE d.workspace_id = $1`
	args := []interface{}{workspaceID}
	if q.TeamID != nil {
		query += fmt.Sprintf(" AND ae.team_id = $%d", len(args)+1)
		args = append(args, *q.TeamID)
	}
	if q.Cursor.Set {
		var cursorCreated time.Time
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM ai_employees WHERE id = $1`, q.Cursor.ID).Scan(&cursorCreated)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return aiemployee.Page{}, aiemployee.ErrInvalidInput
			}
			return aiemployee.Page{}, err
		}
		args = append(args, cursorCreated, q.Cursor.ID)
		query += fmt.Sprintf(" AND (ae.created_at, ae.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, q.Limit+1)
	query += fmt.Sprintf(" ORDER BY ae.created_at DESC, ae.id DESC LIMIT $%d", len(args))

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return aiemployee.Page{}, err
	}
	defer rows.Close()

	var out []*aiemployee.AIEmployee
	for rows.Next() {
		var e aiemployee.AIEmployee
		var tID, dID, wID uuid.UUID
		var name, role string
		var caps pq.StringArray
		var perms, know json.RawMessage
		var memID uuid.NullUUID
		var currTask uuid.NullUUID
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&e.ID, &tID, &dID, &wID, &name, &role, &caps, &perms, &know, &memID, &currTask, &createdAt, &updatedAt); err != nil {
			return aiemployee.Page{}, err
		}
		e.TeamID = tID
		e.DepartmentID = dID
		e.WorkspaceID = wID
		e.Name = name
		e.Role = role
		e.Capabilities = []string(caps)
		if len(perms) > 0 {
			var p any
			_ = json.Unmarshal(perms, &p)
			e.Permissions = p
		}
		if len(know) > 0 {
			var k any
			_ = json.Unmarshal(know, &k)
			e.KnowledgeAccess = k
		}
		if memID.Valid {
			e.MemoryID = &memID.UUID
		}
		if currTask.Valid {
			e.CurrentTaskID = &currTask.UUID
		}
		e.CreatedAt = createdAt
		e.UpdatedAt = updatedAt
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return aiemployee.Page{}, err
	}
	if err := tx.Commit(); err != nil {
		return aiemployee.Page{}, err
	}

	page := aiemployee.Page{Limit: q.Limit}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		page.NextCursor = aiemployee.EncodeCursor(out[len(out)-1])
	}
	page.Employees = out
	return page, nil
}

func (s *AIEmployeeStore) Update(ctx context.Context, workspaceID uuid.UUID, e *aiemployee.AIEmployee) (*aiemployee.AIEmployee, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Verify new team belongs to same workspace.
	var wsID, deptID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT d.workspace_id, t.department_id FROM teams t
		JOIN departments d ON t.department_id = d.id
		WHERE t.id = $1`, e.TeamID).Scan(&wsID, &deptID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, aiemployee.ErrInvalidInput
		}
		return nil, err
	}
	if wsID != workspaceID {
		return nil, aiemployee.ErrWorkspaceMismatch
	}
	e.DepartmentID = deptID
	e.WorkspaceID = wsID

	// Check name uniqueness within team.
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ai_employees WHERE team_id = $1 AND lower(name) = lower($2) AND id != $3)`, e.TeamID, e.Name, e.ID).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, aiemployee.ErrNameTaken
	}

	var updated aiemployee.AIEmployee
	var name, role string
	var caps pq.StringArray
	var perms, know json.RawMessage
	var memID uuid.NullUUID
	var currTask uuid.NullUUID
	var createdAt, updatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE ai_employees SET team_id = $1, name = $2, role = $3, capabilities = $4, permissions = $5, knowledge_access = $6, current_task_id = $7, updated_at = NOW()
		WHERE id = $8
		RETURNING id, team_id, name, role, capabilities, permissions, knowledge_access, memory_id, current_task_id, created_at, updated_at`,
		e.TeamID, e.Name, e.Role, pq.Array(e.Capabilities), jsonValue(e.Permissions), jsonValue(e.KnowledgeAccess), nullableUUID(e.CurrentTaskID), e.ID).Scan(
		&updated.ID, &updated.TeamID, &name, &role, &caps, &perms, &know, &memID, &currTask, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, aiemployee.ErrNotFound
		}
		return nil, err
	}
	updated.Name = name
	updated.Role = role
	updated.Capabilities = []string(caps)
	if len(perms) > 0 {
		var p any
		_ = json.Unmarshal(perms, &p)
		updated.Permissions = p
	}
	if len(know) > 0 {
		var k any
		_ = json.Unmarshal(know, &k)
		updated.KnowledgeAccess = k
	}
	if memID.Valid {
		updated.MemoryID = &memID.UUID
	}
	if currTask.Valid {
		updated.CurrentTaskID = &currTask.UUID
	}
	updated.DepartmentID = deptID
	updated.WorkspaceID = workspaceID
	updated.CreatedAt = createdAt
	updated.UpdatedAt = updatedAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &updated, nil
}

func (s *AIEmployeeStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM ai_employees ae JOIN teams t ON ae.team_id = t.id JOIN departments d ON t.department_id = d.id WHERE ae.id = $1 AND d.workspace_id = $2)`, id, workspaceID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return aiemployee.ErrNotFound
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM ai_employees WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func jsonValue(v any) driver.Value {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

var _ aiemployee.Store = (*AIEmployeeStore)(nil)
