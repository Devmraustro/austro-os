package orchestration

import (
	"context"

	"github.com/google/uuid"
)

// PipelineStore is the workspace-scoped persistence port for pipelines.
// Implementations enforce RLS/workspace isolation so reads and writes never
// cross workspace boundaries.
type PipelineStore interface {
	Create(ctx context.Context, p *Pipeline) (*Pipeline, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Pipeline, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]*Pipeline, error)
	Update(ctx context.Context, workspaceID uuid.UUID, p *Pipeline) (*Pipeline, error)
}

// Capability ports: each pipeline stage depends only on the narrow call it
// needs (interface segregation), and Phase 2 wires deterministic stub adapters
// so no stage performs a real external call (ROADMAP §5.8/§5.9, ADR-010).

// Researcher produces an artefact reference for the research stage. Phase 2
// returns a deterministic, content-derived reference (no live call).
type Researcher interface {
	Research(ctx context.Context, workspaceID uuid.UUID, p *Pipeline) (string, error)
}

// ScriptWriter produces a script/task reference for the script stage.
type ScriptWriter interface {
	WriteScript(ctx context.Context, workspaceID uuid.UUID, p *Pipeline, researchRef string) (string, error)
}

// Reviewer reviews an artefact and reports whether it passed. In Phase 2 the
// review does not bypass the Step 6 human approval gate; it only prepares it.
type Reviewer interface {
	Review(ctx context.Context, workspaceID uuid.UUID, p *Pipeline) (bool, error)
}

// Publisher delivers the reviewed, human-approved publication artefact. It
// returns the produced publication reference. Phase 2 reuses the Step 6
// deterministic stub (no real platform).
type Publisher interface {
	Publish(ctx context.Context, workspaceID uuid.UUID, p *Pipeline, reviewRef string) (string, error)
}

// AuditSink records principle-tagged orchestration decisions.
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord)
}

// AuditRecord is the orchestration service's audit contract.
type AuditRecord struct {
	EventType               string
	ConstitutionalPrinciple string
	Outcome                 string
	WorkspaceID             string
	PipelineID              string
	ActorType               string
	ActorID                 string
	TraceID                 string
	SpanID                  string
	Stage                   string
}

// NullAuditSink discards decisions.
type NullAuditSink struct{}

// Record implements AuditSink by discarding the decision.
func (NullAuditSink) Record(_ context.Context, _ AuditRecord) {}

// EventSink publishes pipeline events (pipeline.research, pipeline.script,
// ...) to the bus for the worker composition loop.
type EventSink interface {
	PublishPipeline(ctx context.Context, eventType string, pipelineID, workspaceID uuid.UUID, stage, traceID, spanID string) error
}

// NullEventSink is a no-op event publisher.
type NullEventSink struct{}

// PublishPipeline implements EventSink as a no-op.
func (NullEventSink) PublishPipeline(_ context.Context, _ string, _ uuid.UUID, _ uuid.UUID, _, _, _ string) error {
	return nil
}
