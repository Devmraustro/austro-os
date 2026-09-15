package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"austro-os/internal/orchestration"
	"github.com/google/uuid"
)

type PipelineStore struct{ db *sql.DB }

func NewPipelineStore(db *sql.DB) *PipelineStore { return &PipelineStore{db: db} }

func (s *PipelineStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_workspace', $1, true)", workspaceID.String()); err != nil {
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
	var version int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO pipelines (workspace_id, goal_id, stage, status, task_id, publication_id,
		  research_reference, script_reference, review_reference, published_reference, failure_reason,
		  retry_count, idempotency_key, approved_by, approved_at, version, trace_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		RETURNING id, created_at, updated_at, version`,
		p.WorkspaceID, nullableUUID(p.GoalID), string(p.Stage), string(p.Status), nullableUUID(p.TaskID), nullableUUID(p.PublicationID),
		nullablePipelineString(p.ResearchReference), nullablePipelineString(p.ScriptReference), nullablePipelineString(p.ReviewReference), nullablePipelineString(p.PublishedReference), nullablePipelineString(p.FailureReason),
		p.RetryCount, nullablePipelineString(p.IdempotencyKey), nullablePipelineString(p.ApprovedBy), nullableTime(p.ApprovedAt), p.Version, nullablePipelineString(p.TraceID), p.CreatedAt, p.UpdatedAt).Scan(&id, &createdAt, &updatedAt, &version)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	p.ID, p.CreatedAt, p.UpdatedAt, p.Version = id, createdAt, updatedAt, version
	return p, nil
}

func (s *PipelineStore) GetByIdempotency(ctx context.Context, workspaceID uuid.UUID, key string) (*orchestration.Pipeline, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := scanPipeline(tx.QueryRowContext(ctx, pipelineSelect+" WHERE workspace_id = $1 AND idempotency_key = $2", workspaceID, key))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PipelineStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*orchestration.Pipeline, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := scanPipeline(tx.QueryRowContext(ctx, pipelineSelect+" WHERE id = $1 AND workspace_id = $2", id, workspaceID))
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
	rows, err := tx.QueryContext(ctx, pipelineSelect+" WHERE workspace_id = $1 ORDER BY created_at DESC, id DESC LIMIT 100", workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*orchestration.Pipeline, 0, 100)
	for rows.Next() {
		p, scanErr := scanPipeline(rows)
		if scanErr != nil {
			return nil, scanErr
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
	previousVersion := p.Version
	if previousVersion < 1 {
		previousVersion = 1
	}
	p.UpdatedAt = time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE pipelines SET stage=$1,status=$2,task_id=$3,publication_id=$4,
		 research_reference=$5,script_reference=$6,review_reference=$7,published_reference=$8,
		 failure_reason=$9,retry_count=$10,idempotency_key=$11,approved_by=$12,approved_at=$13,
		 trace_id=$14,updated_at=$15,version=version+1
		WHERE id=$16 AND workspace_id=$17 AND version=$18`,
		string(p.Stage), string(p.Status), nullableUUID(p.TaskID), nullableUUID(p.PublicationID), nullablePipelineString(p.ResearchReference), nullablePipelineString(p.ScriptReference), nullablePipelineString(p.ReviewReference), nullablePipelineString(p.PublishedReference), nullablePipelineString(p.FailureReason), p.RetryCount, nullablePipelineString(p.IdempotencyKey), nullablePipelineString(p.ApprovedBy), nullableTime(p.ApprovedAt), nullIfEmpty(p.TraceID), p.UpdatedAt, p.ID, workspaceID, previousVersion)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, orchestration.ErrConcurrentUpdate
	}
	p.Version = previousVersion + 1
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

const pipelineSelect = `SELECT id, workspace_id, goal_id, stage, status, task_id, publication_id,
 research_reference, script_reference, review_reference, published_reference, failure_reason,
 retry_count, idempotency_key, approved_by, approved_at, version, trace_id, created_at, updated_at FROM pipelines`

type rowScannerPipe interface {
	Scan(dest ...interface{}) error
}

func scanPipeline(r rowScannerPipe) (*orchestration.Pipeline, error) {
	var p orchestration.Pipeline
	var id, wsID uuid.UUID
	var goalID, taskID, publicationID uuid.NullUUID
	var stage, status string
	var researchRef, scriptRef, reviewRef, publishedRef, failure, key, approvedBy, traceID sql.NullString
	var approvedAt sql.NullTime
	var retry int
	var version int64
	var createdAt, updatedAt time.Time
	if err := r.Scan(&id, &wsID, &goalID, &stage, &status, &taskID, &publicationID, &researchRef, &scriptRef, &reviewRef, &publishedRef, &failure, &retry, &key, &approvedBy, &approvedAt, &version, &traceID, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, orchestration.ErrNotFound
		}
		return nil, err
	}
	p.ID = id
	p.WorkspaceID = wsID
	p.Stage = orchestration.Stage(stage)
	p.Status = orchestration.PipelineStatus(status)
	p.RetryCount = retry
	p.Version = version
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	if goalID.Valid {
		v := goalID.UUID
		p.GoalID = &v
	}
	if taskID.Valid {
		v := taskID.UUID
		p.TaskID = &v
	}
	if publicationID.Valid {
		v := publicationID.UUID
		p.PublicationID = &v
	}
	if researchRef.Valid {
		p.ResearchReference = researchRef.String
	}
	if scriptRef.Valid {
		p.ScriptReference = scriptRef.String
	}
	if reviewRef.Valid {
		p.ReviewReference = reviewRef.String
	}
	if publishedRef.Valid {
		p.PublishedReference = publishedRef.String
	}
	if failure.Valid {
		p.FailureReason = failure.String
	}
	if key.Valid {
		p.IdempotencyKey = key.String
	}
	if approvedBy.Valid {
		p.ApprovedBy = approvedBy.String
	}
	if approvedAt.Valid {
		v := approvedAt.Time
		p.ApprovedAt = &v
	}
	if traceID.Valid {
		p.TraceID = traceID.String
	}
	return &p, nil
}

func nullablePipelineString(v string) interface{} {
	if v == "" {
		return nil
	}
	return v
}
