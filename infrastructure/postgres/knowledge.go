package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"austro-os/internal/knowledge"

	"github.com/google/uuid"
)

// KnowledgeStore is the concrete Postgres adapter for the knowledge.DocumentStore
// port. Each operation runs inside a transaction that first binds
// app.current_workspace on that exact connection, so RLS genuinely constrains
// the operation; explicit workspace_id guards are kept as defense-in-depth.
type KnowledgeStore struct {
	db *sql.DB
}

// NewKnowledgeStore returns a Postgres-backed implementation of
// knowledge.DocumentStore.
func NewKnowledgeStore(db *sql.DB) *KnowledgeStore {
	return &KnowledgeStore{db: db}
}

// beginTx opens an exclusive transaction and binds the workspace context on it
// so RLS resolves correctly for the whole operation.
func (s *KnowledgeStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
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

func (s *KnowledgeStore) Upsert(ctx context.Context, d *knowledge.Document) (*knowledge.Document, error) {
	tx, err := s.beginTx(ctx, d.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	d.UpdatedAt = time.Now().UTC()
	var id uuid.UUID
	var createdAt, updatedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO knowledge_documents (workspace_id, kind, title, content, embedding, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5::vector, $6, $7)
		ON CONFLICT (id) DO UPDATE
			SET kind = EXCLUDED.kind, title = EXCLUDED.title, content = EXCLUDED.content,
			    embedding = EXCLUDED.embedding, updated_at = EXCLUDED.updated_at
		RETURNING id, created_at, updated_at`,
		d.WorkspaceID, string(d.Kind), d.Title, d.Content, vectorLiteral(d.Embedding),
		d.CreatedAt, d.UpdatedAt).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
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

func (s *KnowledgeStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*knowledge.Document, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `
		SELECT id, workspace_id, kind, title, content, embedding::text, created_at, updated_at
		FROM knowledge_documents WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	doc, err := scanDocument(row)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return doc, nil
}

func (s *KnowledgeStore) List(ctx context.Context, workspaceID uuid.UUID, kind *knowledge.Kind) ([]*knowledge.Document, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	query := `
		SELECT id, workspace_id, kind, title, content, embedding::text, created_at, updated_at
		FROM knowledge_documents WHERE workspace_id = $1`
	args := []interface{}{workspaceID}
	if kind != nil {
		query += " AND kind = $2"
		args = append(args, string(*kind))
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*knowledge.Document
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *KnowledgeStore) Search(ctx context.Context, workspaceID uuid.UUID, query []float32, kind *knowledge.Kind, limit int) ([]*knowledge.Document, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sqlQuery := `
		SELECT id, workspace_id, kind, title, content, embedding::text, created_at, updated_at
		FROM knowledge_documents
		WHERE workspace_id = $1`
	args := []interface{}{workspaceID}
	param := 2
	if kind != nil {
		sqlQuery += fmt.Sprintf(" AND kind = $%d", param)
		args = append(args, string(*kind))
		param++
	}
	sqlQuery += fmt.Sprintf(" ORDER BY embedding <=> $%d::vector LIMIT $%d", param, param+1)
	args = append(args, vectorLiteral(query), limit)

	rows, err := tx.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*knowledge.Document
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *KnowledgeStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		"DELETE FROM knowledge_documents WHERE id = $1 AND workspace_id = $2", id, workspaceID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return knowledge.ErrNotFound
	}
	return tx.Commit()
}

// scanDocument reads a knowledge document row. embedding::text is scanned and
// parsed back into a float32 vector.
func scanDocument(r rowScanner) (*knowledge.Document, error) {
	var d knowledge.Document
	var id, wsID uuid.UUID
	var kind, title, content string
	var embeddingText sql.NullString
	var createdAt, updatedAt time.Time
	if err := r.Scan(&id, &wsID, &kind, &title, &content, &embeddingText, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, knowledge.ErrNotFound
		}
		return nil, err
	}
	d.ID = id
	d.WorkspaceID = wsID
	d.Kind = knowledge.Kind(kind)
	d.Title = title
	d.Content = content
	if embeddingText.Valid {
		d.Embedding = parseVector(embeddingText.String)
	}
	d.CreatedAt = createdAt
	d.UpdatedAt = updatedAt
	return &d, nil
}

// parseVector converts a PGVector text literal '[1,2,3]' into a float32 slice.
func parseVector(s string) []float32 {
	trimmed := strings.Trim(strings.TrimSpace(s), "[]")
	if trimmed == "" {
		return []float32{}
	}
	parts := strings.Split(trimmed, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%f", &f); err != nil {
			return nil
		}
		out = append(out, float32(f))
	}
	return out
}
