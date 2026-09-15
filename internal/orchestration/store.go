package orchestration

import (
	"context"

	"github.com/google/uuid"
)

// PipelineStore is the workspace-scoped persistence port for pipelines.
// Implementations enforce RLS/workspace isolation so reads and writes never
// cross workspace boundaries. List implementations must return a bounded,
// deterministic page (newest first).
type PipelineStore interface {
	Create(ctx context.Context, p *Pipeline) (*Pipeline, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Pipeline, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]*Pipeline, error)
	Update(ctx context.Context, workspaceID uuid.UUID, p *Pipeline) (*Pipeline, error)
}

// IdempotentPipelineStore is an optional extension implemented by persistent
// stores. Keeping it out of PipelineStore preserves small test/domain fakes,
// while the service uses it whenever the adapter supports a durable key lookup.
type IdempotentPipelineStore interface {
	PipelineStore
	GetByIdempotency(ctx context.Context, workspaceID uuid.UUID, key string) (*Pipeline, error)
}

// Capability ports: each pipeline stage depends only on the narrow call it needs.
type Researcher interface {
	Research(ctx context.Context, workspaceID uuid.UUID, p *Pipeline) (string, error)
}

type ScriptWriter interface {
	WriteScript(ctx context.Context, workspaceID uuid.UUID, p *Pipeline, researchRef string) (string, error)
}

type Reviewer interface {
	Review(ctx context.Context, workspaceID uuid.UUID, p *Pipeline) (bool, error)
}

type Publisher interface {
	Publish(ctx context.Context, workspaceID uuid.UUID, p *Pipeline, reviewRef string) (string, error)
}

// PublicationPort is the narrow bridge to the already-existing Publishing
// service. It prepares a real persisted publication, records human approval,
// and delivers it; orchestration does not import or duplicate that domain.
type PublicationPort interface {
	Prepare(ctx context.Context, workspaceID uuid.UUID, p *Pipeline) (uuid.UUID, error)
	Approve(ctx context.Context, workspaceID, publicationID uuid.UUID, actorID string) error
	Publish(ctx context.Context, workspaceID, publicationID uuid.UUID, actorID string) (string, error)
}

// AuditSink records principle-tagged orchestration decisions.
type AuditSink interface { Record(ctx context.Context, rec AuditRecord) }

type AuditRecord struct {
	EventType string
	ConstitutionalPrinciple string
	Outcome string
	WorkspaceID string
	PipelineID string
	ActorType string
	ActorID string
	TraceID string
	SpanID string
	Stage string
}

type NullAuditSink struct{}
func (NullAuditSink) Record(_ context.Context, _ AuditRecord) {}

type EventSink interface {
	PublishPipeline(ctx context.Context, eventType string, pipelineID, workspaceID uuid.UUID, stage, traceID, spanID string) error
}

type NullEventSink struct{}
func (NullEventSink) PublishPipeline(_ context.Context, _ string, _ uuid.UUID, _ uuid.UUID, _, _, _ string) error { return nil }
