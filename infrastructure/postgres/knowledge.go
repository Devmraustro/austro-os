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

// vectorLiteral encodes a float32 embedding as the PGVector literal format
// '[1,2,3]' used in the ::vector column casts.
func vectorLiteral(embedding []float32) string {
	parts := make([]string, 0, len(embedding))
	for _, v := range embedding {
		parts = append(parts, fmt.Sprintf("%g", v))
	}
	return "[" + strings.Join(parts, ",") + "]"
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

// Update modifies an existing document. It is a real UPDATE rather than the
// Upsert conflict path: routing an update through INSERT … ON CONFLICT would
// resurrect a document deleted between the caller's read and this write, turning
// a concurrent delete into a silent recreate. Zero affected rows is therefore
// reported as not found.
func (s *KnowledgeStore) Update(ctx context.Context, d *knowledge.Document) (*knowledge.Document, error) {
	tx, err := s.beginTx(ctx, d.WorkspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	d.UpdatedAt = time.Now().UTC()
	var id uuid.UUID
	var createdAt, updatedAt time.Time
	res := tx.QueryRowContext(ctx, `
		UPDATE knowledge_documents
		SET kind = $3, title = $4, content = $5, embedding = $6::vector, updated_at = $7
		WHERE id = $1 AND workspace_id = $2
		RETURNING id, created_at, updated_at`,
		d.ID, d.WorkspaceID, string(d.Kind), d.Title, d.Content,
		vectorLiteral(d.Embedding), d.UpdatedAt)
	if err := res.Scan(&id, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, knowledge.ErrNotFound
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	d.CreatedAt = createdAt
	d.UpdatedAt = updatedAt
	return d, nil
}

// ListPage returns one bounded page of documents, newest first.
//
// The ordering carries the id as a tie-break because created_at alone can repeat:
// two documents written in the same microsecond would tie, and a tie at a page
// boundary makes the boundary arbitrary, so a cursor walk could skip or repeat a
// row. The same pair is what the cursor encodes, which is what keeps the walk
// exact.
//
// limit+1 rows are fetched so that "there is a next page" is known rather than
// inferred from a page that happened to come back full.
func (s *KnowledgeStore) ListPage(ctx context.Context, workspaceID uuid.UUID, q knowledge.ListQuery) (knowledge.Page, error) {
	q.Normalize()
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return knowledge.Page{}, err
	}
	defer tx.Rollback()

	// The workspace predicate is explicit even though row-level security already
	// confines the query: the two are independent, and if one is ever weakened
	// the other still holds.
	query := `
		SELECT id, workspace_id, kind, title, content, embedding::text, created_at, updated_at
		FROM knowledge_documents WHERE workspace_id = $1`
	args := []interface{}{workspaceID}
	next := 2
	if q.Kind != nil {
		query += fmt.Sprintf(" AND kind = $%d", next)
		args = append(args, string(*q.Kind))
		next++
	}
	if q.Before.Set {
		// Strictly after the cursor in (created_at DESC, id DESC). The second
		// term is what makes the boundary exact when timestamps tie.
		query += fmt.Sprintf(
			" AND (created_at, id) < ($%d, $%d)", next, next+1)
		args = append(args, q.Before.CreatedAt, q.Before.ID)
		next += 2
	}
	query += fmt.Sprintf(
		" ORDER BY created_at DESC, id DESC LIMIT $%d", next)
	args = append(args, q.Limit+1)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return knowledge.Page{}, err
	}
	defer rows.Close()

	var docs []*knowledge.Document
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return knowledge.Page{}, err
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return knowledge.Page{}, err
	}
	if err := tx.Commit(); err != nil {
		return knowledge.Page{}, err
	}

	page := knowledge.Page{Limit: q.Limit}
	if len(docs) > q.Limit {
		docs = docs[:q.Limit]
		// The cursor points at the last row actually returned, so the next page
		// starts strictly after it.
		page.NextCursor = knowledge.EncodeCursor(docs[len(docs)-1])
	}
	page.Documents = docs
	return page, nil
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
