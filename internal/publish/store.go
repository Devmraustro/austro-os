package publish

import (
	"context"

	"github.com/google/uuid"
)

// PublicationStore is the workspace-scoped persistence port for publications.
// Implementations must enforce RLS/workspace isolation so reads and writes
// never cross workspace boundaries. It is implemented by an infrastructure
// adapter; the domain never depends on the adapter.
type PublicationStore interface {
	// Create persists a new (queued) publication and returns it.
	Create(ctx context.Context, p *Publication) (*Publication, error)
	// Get returns a publication by id within the given workspace.
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Publication, error)
	// List returns publications in a workspace, optionally filtered by status.
	List(ctx context.Context, workspaceID uuid.UUID, status *Status) ([]*Publication, error)
	// Update persists field/status changes for a publication owned by the
	// workspace.
	Update(ctx context.Context, workspaceID uuid.UUID, p *Publication) (*Publication, error)
}

// Publisher is the replaceable platform adapter (Replaceability, P6). A
// publication is only ever handed to the publisher after human approval. The
// Phase 2 adapter is a deterministic stub: it never contacts a real platform,
// holds no credential, and performs no live external write (ADR-009, ROADMAP
// §5.8/§5.10).
type Publisher interface {
	// Publish delivers an approved publication to the platform and returns the
	// adapter's stable external reference (empty for the stub). Implementations
	// must be deterministic and side-effect free beyond recording.
	Publish(ctx context.Context, p *Publication) (string, error)
}

// AuditSink is the audit-compatible decision sink used by the publishing
// service. It carries only non-secret metadata for an append-only,
// principle-tagged decision. A concrete adapter connects this to the audit
// chain at the composition boundary.
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord)
}

// AuditRecord is the publishing service's audit contract.
type AuditRecord struct {
	EventType               string
	ConstitutionalPrinciple string
	Outcome                 string
	WorkspaceID             string
	PublicationID           string
	ActorType               string
	ActorID                 string
	TraceID                 string
	SpanID                  string
}

// NullAuditSink discards decisions (default when none is injected).
type NullAuditSink struct{}

// Record implements AuditSink by discarding the decision.
func (NullAuditSink) Record(_ context.Context, _ AuditRecord) {}

// EventSink publishes domain events (e.g. publication.created,
// publication.approved, publication.published) to an event bus. Domain stays
// behind the port.
type EventSink interface {
	Publish(ctx context.Context, eventType string, publicationID, workspaceID uuid.UUID, traceID, spanID string) error
}

// NullEventSink is a no-op publisher.
type NullEventSink struct{}

// Publish implements EventSink as a no-op.
func (NullEventSink) Publish(_ context.Context, _ string, _ uuid.UUID, _ uuid.UUID, _, _ string) error {
	return nil
}
