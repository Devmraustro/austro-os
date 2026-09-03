// Package knowledge provides the Knowledge Management domain for the Creator
// pipeline.
//
// Knowledge is workspace-scoped company knowledge, campaign rules, style guides
// and documentation. Documents are stored with PGVector embeddings and isolated
// by RLS per workspace (Privacy by Design). Embeddings are produced by the
// provider-agnostic AI gateway (ADR-004). This package defines the Document
// model, a workspace-scoped persistence port (DocumentStore), an Embedder port,
// and a Service that embeds on create, enforces workspace ownership, and emits
// audit events and structured logs. It imports no infrastructure package
// (Phase 1 criterion #32 preserves dependency direction).
package knowledge

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels reported by the knowledge boundary.
var (
	ErrInvalidInput      = errors.New("knowledge: invalid input")
	ErrNotFound          = errors.New("knowledge: not found")
	ErrWorkspaceMismatch = errors.New("knowledge: workspace mismatch")
	ErrInvalidKind       = errors.New("knowledge: invalid kind")
	ErrTooLarge          = errors.New("knowledge: content exceeds the maximum length")
)

// maxContentLength caps a single knowledge document body (abuse control /
// upload and embed limit, ROADMAP §8 knowledge row).
const maxContentLength = 64 * 1024

// Kind classifies a knowledge document. Kinds are stored as text and validated
// here (schema additive).
type Kind string

const (
	KindDocument     Kind = "document"
	KindCampaignRule Kind = "campaign_rule"
	KindStyleGuide   Kind = "style_guide"
)

// DefaultEmbeddingDimensions matches the Phase 1 PGVector dimension (10) used by
// memory_embeddings, so a single deterministic stub and the <=> operator apply.
const DefaultEmbeddingDimensions = 10

// Document is a workspace-scoped knowledge record with an embedding vector.
type Document struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Kind        Kind      `json:"kind"`
	Title       string    `json:"title"`
	Content     string    `json:"content"`
	Embedding   []float32 `json:"embedding,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ValidKind reports whether k is a recognized kind.
func ValidKind(k Kind) bool {
	switch k {
	case KindDocument, KindCampaignRule, KindStyleGuide:
		return true
	}
	return false
}

// New validates inputs and returns a knowledge document as a value aggregate
// (not yet persisted). WorkspaceID must be non-empty; title/content non-empty
// and within length limits; kind recognized.
func New(workspaceID uuid.UUID, kind Kind, title, content string) (*Document, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	if !ValidKind(kind) {
		return nil, ErrInvalidKind
	}
	if strings.TrimSpace(title) == "" {
		return nil, ErrInvalidInput
	}
	if strings.TrimSpace(content) == "" {
		return nil, ErrInvalidInput
	}
	if len(content) > maxContentLength {
		return nil, ErrTooLarge
	}
	now := time.Now().UTC()
	return &Document{
		ID:          uuid.New(),
		WorkspaceID: workspaceID,
		Kind:        kind,
		Title:       strings.TrimSpace(title),
		Content:     content,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}
