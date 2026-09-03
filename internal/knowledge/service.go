package knowledge

import (
	"context"
	"strings"

	logger "austro-os/internal/log"
	"github.com/google/uuid"
)

// Service is the knowledge application service. It embeds on create, enforces
// workspace ownership (deny-by-default), and emits audit events and structured
// logs with optional trace/span propagation.
type Service struct {
	store  DocumentStore
	embed  Embedder
	audit  AuditSink
	events EventSink
	dim    int
}

// NewService wires a Service from its ports. A nil audit or event sink is
// replaced by a no-op. dim is the embedding dimension (0 → the domain default).
func NewService(store DocumentStore, embed Embedder, audit AuditSink, events EventSink, dim int) *Service {
	if audit == nil {
		audit = NullAuditSink{}
	}
	if events == nil {
		events = NullEventSink{}
	}
	if dim <= 0 {
		dim = DefaultEmbeddingDimensions
	}
	return &Service{store: store, embed: embed, audit: audit, events: events, dim: dim}
}

// Create validates a document, embeds its content, and persists it.
func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, kind Kind, title, content string) (*Document, error) {
	d, err := New(workspaceID, kind, title, content)
	if err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "knowledge.create", ConstitutionalPrinciple: "Vision First",
			Outcome: "failed", WorkspaceID: workspaceID.String(), ActorType: "system",
		})
		return nil, err
	}
	id := d.ID
	vec, err := s.embed.Embed(ctx, workspaceID, d.Content, s.dim)
	if err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "knowledge.create", ConstitutionalPrinciple: "AI Independence",
			Outcome: "failed", WorkspaceID: workspaceID.String(), DocumentID: id.String(), ActorType: "system",
		})
		return nil, err
	}
	d.Embedding = vec
	stored, err := s.store.Upsert(ctx, d)
	if err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "knowledge.create", ConstitutionalPrinciple: "Vision First",
			Outcome: "failed", WorkspaceID: workspaceID.String(), DocumentID: id.String(), ActorType: "system",
		})
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{
		EventType: "knowledge.create", ConstitutionalPrinciple: "Vision First",
		Outcome: "success", WorkspaceID: workspaceID.String(), DocumentID: stored.ID.String(), ActorType: "system",
	})
	_ = s.events.Publish(ctx, "knowledge.created", stored.ID, workspaceID, traceOf(ctx), spanOf(ctx))
	log(workspaceID, "knowledge-created").With("document_id", stored.ID).With("kind", stored.Kind).Log()
	return stored, nil
}

// Get returns a document by id within the workspace.
func (s *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Document, error) {
	d, err := s.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if d.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	return d, nil
}

// List returns documents in the workspace, optionally filtered by kind.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID, kind *Kind) ([]*Document, error) {
	return s.store.List(ctx, workspaceID, kind)
}

// Search returns the top-k nearest documents in the workspace by embedding
// similarity. The caller supplies the query text; the embedder converts it.
func (s *Service) Search(ctx context.Context, workspaceID uuid.UUID, query string, kind *Kind, limit int) ([]*Document, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	if strings.TrimSpace(query) == "" {
		return nil, ErrInvalidInput
	}
	qv, err := s.embed.Embed(ctx, workspaceID, strings.TrimSpace(query), s.dim)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	docs, err := s.store.Search(ctx, workspaceID, qv, kind, limit)
	if err != nil {
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{
		EventType: "knowledge.search", ConstitutionalPrinciple: "Privacy by Design",
		Outcome: "success", WorkspaceID: workspaceID.String(), ActorType: "system",
		TraceID: traceOf(ctx), SpanID: spanOf(ctx),
	})
	return docs, nil
}

// Delete removes a document within the workspace.
func (s *Service) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	if err := s.store.Delete(ctx, workspaceID, id); err != nil {
		return err
	}
	s.audit.Record(ctx, AuditRecord{
		EventType: "knowledge.delete", ConstitutionalPrinciple: "Security by Design",
		Outcome: "success", WorkspaceID: workspaceID.String(), DocumentID: id.String(), ActorType: "system",
	})
	return nil
}

func log(workspaceID uuid.UUID, msg string) *logger.Entry {
	return logger.NewEntry(msg).With("workspace_id", workspaceID)
}

type traceKey struct{}
type spanKey struct{}

// WithTrace attaches a trace id to ctx for audit propagation.
func WithTrace(ctx context.Context, trace, span string) context.Context {
	ctx = context.WithValue(ctx, traceKey{}, trace)
	ctx = context.WithValue(ctx, spanKey{}, span)
	return ctx
}

func traceOf(ctx context.Context) string {
	if v, ok := ctx.Value(traceKey{}).(string); ok {
		return v
	}
	return ""
}

func spanOf(ctx context.Context) string {
	if v, ok := ctx.Value(spanKey{}).(string); ok {
		return v
	}
	return ""
}
