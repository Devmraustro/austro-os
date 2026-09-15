package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"austro-os/internal/publish"

	"github.com/google/uuid"
)

// PublicationStore is the concrete Postgres adapter for the
// publish.PublicationStore port. Each operation runs inside a transaction that
// first binds app.current_workspace on that exact connection, so row-level
// security genuinely constrains the operation. Explicit workspace_id guards are
// kept as defense-in-depth.
type PublicationStore struct {
	db *sql.DB
}

// NewPublicationStore returns a Postgres-backed implementation of
// publish.PublicationStore.
func NewPublicationStore(db *sql.DB) *PublicationStore {
	return &PublicationStore{db: db}
}

// beginTx opens an exclusive transaction and binds the workspace context on it
// so RLS resolves correctly for the whole operation.
func (s *PublicationStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
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

func (s *PublicationStore) Create(ctx context.Context, p *publish.Publication) (*publish.Publication, error) {
	tx, err := s.beginTx(ctx, p.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id uuid.UUID
	var createdAt, updatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO publications (workspace_id, goal_id, task_id, title, body, platform,
		                          status, content_hash, idempotency_key, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, created_at, updated_at`,
		p.WorkspaceID, nullableUUID(p.GoalID), nullableUUID(p.TaskID), p.Title, p.Body,
		p.Platform, string(p.Status), p.ContentHash, nullableString(p.IdempotencyKey), p.CreatedAt, p.UpdatedAt).Scan(&id, &createdAt, &updatedAt)
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

// GetByIdempotency resolves a create retry inside the caller's workspace.
func (s *PublicationStore) GetByIdempotency(ctx context.Context, workspaceID uuid.UUID, key string) (*publish.Publication, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil { return nil, err }
	defer tx.Rollback()
	p, err := scanPublication(tx.QueryRowContext(ctx, `
		SELECT id, workspace_id, goal_id, task_id, title, body, platform, status, content_hash,
		       idempotency_key, external_reference, failure_reason, approved_by, approved_at,
		       rejected_by, rejected_at, published_at, published_by, created_at, updated_at
		FROM publications WHERE workspace_id = $1 AND idempotency_key = $2`, workspaceID, key))
	if err != nil { return nil, err }
	if err := tx.Commit(); err != nil { return nil, err }
	return p, nil
}

func (s *PublicationStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*publish.Publication, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	p, err := scanPublication(tx.QueryRowContext(ctx, `
		SELECT id, workspace_id, goal_id, task_id, title, body, platform, status, content_hash,
		       idempotency_key, external_reference, failure_reason, approved_by, approved_at,
		       rejected_by, rejected_at, published_at, published_by, created_at, updated_at
		FROM publications WHERE id = $1 AND workspace_id = $2`, id, workspaceID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PublicationStore) List(ctx context.Context, workspaceID uuid.UUID, status *publish.Status) ([]*publish.Publication, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	query := `
		SELECT id, workspace_id, goal_id, task_id, title, body, platform, status, content_hash,
		       idempotency_key, external_reference, failure_reason, approved_by, approved_at,
		       rejected_by, rejected_at, published_at, published_by, created_at, updated_at
		FROM publications WHERE workspace_id = $1`
	args := []interface{}{workspaceID}
	if status != nil {
		query += " AND status = $2"
		args = append(args, string(*status))
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT 100"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*publish.Publication
	for rows.Next() {
		p, err := scanPublication(rows)
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

func (s *PublicationStore) Update(ctx context.Context, workspaceID uuid.UUID, p *publish.Publication) (*publish.Publication, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	p.UpdatedAt = time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		UPDATE publications SET status = $1, idempotency_key = $2, external_reference = $3,
		       failure_reason = $4, approved_by = $5, approved_at = $6,
		       rejected_by = $7, rejected_at = $8, published_at = $9, published_by = $10, updated_at = $11
		WHERE id = $12 AND workspace_id = $13`,
		string(p.Status), nullableString(p.IdempotencyKey), nullableString(p.ExternalReference),
		nullableString(p.FailureReason), nullableStringP(p.ApprovedBy), nullableTime(p.ApprovedAt),
		nullableStringP(p.RejectedBy), nullableTime(p.RejectedAt), nullableTime(p.PublishedAt), nullableStringP(p.PublishedBy),
		p.UpdatedAt, p.ID, workspaceID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

type rowScannerP interface {
	Scan(dest ...interface{}) error
}

func scanPublication(r rowScannerP) (*publish.Publication, error) {
	var p publish.Publication
	var id, wsID uuid.UUID
	var goalID, taskID uuid.NullUUID
	var title, body, platform, status, contentHash string
	var idempotencyKey, externalReference, failureReason sql.NullString
	var approvedBy, rejectedBy, publishedBy sql.NullString
	var approvedAt, rejectedAt, publishedAt sql.NullTime
	var createdAt, updatedAt time.Time
	if err := r.Scan(&id, &wsID, &goalID, &taskID, &title, &body, &platform, &status, &contentHash,
		&idempotencyKey, &externalReference, &failureReason, &approvedBy, &approvedAt,
		&rejectedBy, &rejectedAt, &publishedAt, &publishedBy, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, publish.ErrNotFound
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
	p.Title = title
	p.Body = body
	p.Platform = platform
	p.Status = publish.Status(status)
	p.ContentHash = contentHash
	if idempotencyKey.Valid { p.IdempotencyKey = idempotencyKey.String }
	if externalReference.Valid { p.ExternalReference = externalReference.String }
	if failureReason.Valid { p.FailureReason = failureReason.String }
	if approvedBy.Valid {
		p.ApprovedBy = &approvedBy.String
	}
	if approvedAt.Valid {
		p.ApprovedAt = &approvedAt.Time
	}
	if rejectedBy.Valid {
		p.RejectedBy = &rejectedBy.String
	}
	if rejectedAt.Valid {
		p.RejectedAt = &rejectedAt.Time
	}
	if publishedAt.Valid {
		p.PublishedAt = &publishedAt.Time
	}
	if publishedBy.Valid { p.PublishedBy = &publishedBy.String }
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	return &p, nil
}

// nullableString converts an optional string value to a SQL NULL when empty.
func nullableString(s string) sql.NullString {
	if s == "" { return sql.NullString{} }
	return sql.NullString{String: s, Valid: true}
}

// nullableStringP converts a *string to sql.NullString for scan-safe columns.
func nullableStringP(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}
