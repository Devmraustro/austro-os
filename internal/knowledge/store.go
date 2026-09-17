package knowledge

import (
	"context"

	"github.com/google/uuid"
)

// DocumentStore is the workspace-scoped persistence port for knowledge
// documents. Implementations must enforce RLS/workspace isolation so reads and
// writes never cross workspace boundaries. It is implemented by an
// infrastructure adapter; the domain never depends on the adapter.
type DocumentStore interface {
	// Upsert persists a document (with its embedding) and returns it. This is the
	// create path: it is INSERT … ON CONFLICT (id) DO UPDATE, so it must not be
	// used to modify an existing document, because a row deleted between the
	// caller's read and this write would be resurrected rather than reported
	// missing. Use Update for that.
	Upsert(ctx context.Context, d *Document) (*Document, error)
	// Update modifies the mutable fields of an existing document within the
	// workspace and returns the stored row. It reports ErrNotFound when no row
	// matched, so a concurrent delete surfaces as not-found rather than as a
	// silently recreated document.
	Update(ctx context.Context, d *Document) (*Document, error)
	// Get returns a document by id within the given workspace.
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Document, error)
	// List returns documents in a workspace, optionally filtered by kind.
	//
	// Deprecated for API use: it is unbounded and unordered, which makes it both
	// a denial-of-service vector and non-deterministic. ListPage is the bounded
	// contract; this method is retained for the worker and test paths that want
	// a whole small workspace.
	List(ctx context.Context, workspaceID uuid.UUID, kind *Kind) ([]*Document, error)
	// ListPage returns one bounded page of documents in newest-first order,
	// optionally filtered by kind, positioned by a keyset cursor.
	ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error)
	// Search returns the top-k documents by cosine similarity to the query
	// embedding, restricted to the caller's workspace.
	Search(ctx context.Context, workspaceID uuid.UUID, query []float32, kind *Kind, limit int) ([]*Document, error)
	// Delete removes a document within a workspace.
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

// Embedder produces a fixed-size embedding vector for a text body. The Step 2
// AI gateway's Provider.Embed (via an adapter that preserves workspace scope)
// implements this port. It stays a port so the domain never depends on the AI
// package or any real provider (ADR-004, ROADMAP §5.7).
type Embedder interface {
	Embed(ctx context.Context, workspaceID uuid.UUID, content string, dimensions int) ([]float32, error)
}

// AuditSink records knowledge decisions. A concrete adapter connects this to
// the audit chain at the composition boundary.
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord)
}

// AuditRecord is the knowledge service's audit contract. It carries only
// non-secret metadata for an append-only, principle-tagged decision.
type AuditRecord struct {
	EventType               string
	ConstitutionalPrinciple string
	Outcome                 string
	WorkspaceID             string
	DocumentID              string
	ActorType               string
	ActorID                 string
	TraceID                 string
	SpanID                  string
}

// NullAuditSink discards decisions (default when none is injected).
type NullAuditSink struct{}

// Record implements AuditSink by discarding the decision.
func (NullAuditSink) Record(_ context.Context, _ AuditRecord) {}

// EventSink publishes domain events (e.g. knowledge.created, knowledge.deleted)
// to an event bus for downstream consumers.
type EventSink interface {
	Publish(ctx context.Context, eventType string, documentID uuid.UUID, workspaceID uuid.UUID, traceID, spanID string) error
}

// NullEventSink is a no-op publisher.
type NullEventSink struct{}

// Publish implements EventSink as a no-op.
func (NullEventSink) Publish(_ context.Context, _ string, _ uuid.UUID, _ uuid.UUID, _, _ string) error {
	return nil
}
