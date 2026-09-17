package knowledge

import (
	"context"
	"strings"
	"time"

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

// Update modifies the mutable fields of an existing document.
//
// Fields are pointers so that "absent" is distinguishable from "set to empty":
// a PATCH that omits the title must leave the title alone. Every supplied value
// is validated by the same rules a create goes through, so an update cannot be
// used to write a document that a create would have refused.
//
// The content is re-embedded only when it actually changes. Re-embedding on a
// title-only edit would spend an AI call to produce a vector from unchanged
// input, and the vector is what search ranks on, so it must track the content
// and nothing else.
func (s *Service) Update(ctx context.Context, workspaceID uuid.UUID, id uuid.UUID,
	title, content *string, kind *Kind) (*Document, error) {

	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	existing, err := s.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	// The store is workspace-scoped, but the document carries its own workspace
	// and comparing it is cheap: a store that ever returned the wrong row would
	// be caught here rather than written back to.
	if existing.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}

	next := *existing
	if kind != nil {
		if !ValidKind(*kind) {
			s.audit.Record(ctx, AuditRecord{
				EventType: "knowledge.update", ConstitutionalPrinciple: "Vision First",
				Outcome: "failed", WorkspaceID: workspaceID.String(), DocumentID: id.String(),
				ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
			})
			return nil, ErrInvalidKind
		}
		next.Kind = *kind
	}
	if title != nil {
		if strings.TrimSpace(*title) == "" {
			s.audit.Record(ctx, AuditRecord{
				EventType: "knowledge.update", ConstitutionalPrinciple: "Vision First",
				Outcome: "failed", WorkspaceID: workspaceID.String(), DocumentID: id.String(),
				ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
			})
			return nil, ErrInvalidInput
		}
		next.Title = strings.TrimSpace(*title)
	}
	contentChanged := false
	if content != nil {
		if strings.TrimSpace(*content) == "" {
			s.audit.Record(ctx, AuditRecord{
				EventType: "knowledge.update", ConstitutionalPrinciple: "Vision First",
				Outcome: "failed", WorkspaceID: workspaceID.String(), DocumentID: id.String(),
				ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
			})
			return nil, ErrInvalidInput
		}
		if len(*content) > maxContentLength {
			s.audit.Record(ctx, AuditRecord{
				EventType: "knowledge.update", ConstitutionalPrinciple: "Vision First",
				Outcome: "failed", WorkspaceID: workspaceID.String(), DocumentID: id.String(),
				ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
			})
			return nil, ErrTooLarge
		}
		contentChanged = *content != existing.Content
		next.Content = *content
	}

	// The service sets updated_at rather than leaving it to the store. An
	// invariant that only one adapter happens to enforce is not an invariant: a
	// second implementation, or a test double, would return a document whose
	// timestamp says it was never touched.
	next.UpdatedAt = time.Now().UTC()

	if contentChanged {
		vec, err := s.embed.Embed(ctx, workspaceID, next.Content, s.dim)
		if err != nil {
			s.audit.Record(ctx, AuditRecord{
				EventType: "knowledge.update", ConstitutionalPrinciple: "AI Independence",
				Outcome: "failed", WorkspaceID: workspaceID.String(), DocumentID: id.String(),
				ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
			})
			return nil, err
		}
		next.Embedding = vec
	}

	stored, err := s.store.Update(ctx, &next)
	if err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "knowledge.update", ConstitutionalPrinciple: "Vision First",
			Outcome: "failed", WorkspaceID: workspaceID.String(), DocumentID: id.String(),
			ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
		})
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{
		EventType: "knowledge.update", ConstitutionalPrinciple: "Vision First",
		Outcome: "success", WorkspaceID: workspaceID.String(), DocumentID: stored.ID.String(),
		ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
	})
	_ = s.events.Publish(ctx, "knowledge.updated", stored.ID, workspaceID, traceOf(ctx), spanOf(ctx))
	log(workspaceID, "knowledge-updated").With("document_id", stored.ID).Log()
	return stored, nil
}

// ListPage returns one bounded page of the workspace's documents, newest first.
//
// It is the API-facing listing. List is left in place for callers that genuinely
// want a whole small workspace, but it has no bound and no order, so it is not
// safe to put behind a route.
func (s *Service) ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error) {
	if workspaceID == uuid.Nil {
		return Page{}, ErrWorkspaceMismatch
	}
	q.Normalize()
	return s.store.ListPage(ctx, workspaceID, q)
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
