package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"austro-os/internal/orchestration"

	"github.com/google/uuid"
)

// PipelineStore is the concrete Postgres adapter for the
// orchestration.PipelineStore port. Each operation runs inside a transaction
// that first binds app.current_workspace on that connection so row-level
// security genuinely constrains the operation. Explicit workspace_id guards are
// kept as defense-in-depth.
type PipelineStore struct {
	db *sql.DB
}

// NewPipelineStore returns a Postgres-backed implementation of
// orchestration.PipelineStore.
func NewPipelineStore(db *sql.DB) *PipelineStore {
	return &PipelineStore{db: db}
}

// beginTx opens an exclusive transaction and binds the workspace context on it
// so RLS resolves correctly for the whole operation.
func (s *PipelineStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
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

func (s *PipelineStore) Create(ctx context.Context, p *orchestration.Pipeline) (*orchestration.Pipeline, error) {
	tx, err := s.beginTx(ctx, p.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id uuid.UUID
	var createdAt, updatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO pipelines (workspace_id, goal_id, stage, status, task_id, publication_id,
		                      trace_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at, updated_at`,
		p.WorkspaceID, nullableUUID(p.GoalID), string(p.Stage), string(p.Status),
		nullableUUID(p.TaskID), nullableUUID(p.PublicationID), nullIfEmpty(p.TraceID),
		p.CreatedAt, p.UpdatedAt).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.ID = id
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	return p, nil
}

func (s *PipelineStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*orchestration.Pipeline, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	p, err := scanPipeline(tx.QueryRowContext(ctx, `
		SELECT id, workspace_id, goal_id, stage, status, task_id, publication_id,
		       trace_id, created_at, updated_at
		FROM pipelines WHERE id = $1 AND workspace_id = $2`, id, workspaceID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PipelineStore) List(ctx context.Context, workspaceID uuid.UUID) ([]*orchestration.Pipeline, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, workspace_id, goal_id, stage, status, task_id, publication_id,
		       trace_id, created_at, updated_at
		FROM pipelines WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*orchestration.Pipeline
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PipelineStore) Update(ctx context.Context, workspaceID uuid.UUID, p *orchestration.Pipeline) (*orchestration.Pipeline, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	p.UpdatedAt = time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		UPDATE pipelines SET stage = $1, status = $2, task_id = $3, publication_id = $4,
		       trace_id = $5, updated_at = $6
		WHERE id = $7 AND workspace_id = $8`,
		string(p.Stage), string(p.Status), nullableUUID(p.TaskID), nullableUUID(p.PublicationID),
		nullIfEmpty(p.TraceID), p.UpdatedAt, p.ID, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

type rowScannerPipe interface {
	Scan(dest ...interface{}) error
}

func scanPipeline(r rowScannerPipe) (*orchestration.Pipeline, error) {
	var p orchestration.Pipeline
	var id, wsID uuid.UUID
	var goalID, taskID, publicationID uuid.NullUUID
	var stage, status, traceID string
	var createdAt, updatedAt time.Time
	if err := r.Scan(&id, &wsID, &goalID, &stage, &status, &taskID, &publicationID,
		&traceID, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, orchestration.ErrNotFound
		}
		return nil, err
	}
	p.ID = id
	p.WorkspaceID = wsID
	if goalID.Valid {
		g := goalID.UUID
		p.GoalID = &g
	}
	if taskID.Valid {
		t := taskID.UUID
		p.TaskID = &t
	}
	if publicationID.Valid {
		pc := publicationID.UUID
		p.PublicationID = &pc
	}
	p.Stage = orchestration.Stage(stage)
	p.Status = orchestration.PipelineStatus(status)
	p.TraceID = traceID
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	return &p, nil
}

// nullIfEmpty returns nil for an empty string so it maps to a NULL trace_id.
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
