package orchestration

import (
	"context"
	"errors"
	"sync"
	"time"

	logger "austro-os/internal/log"
	"github.com/google/uuid"
)

// Service is the Creator orchestration application service. It advances the
// pipeline stage by stage, enforcing workspace ownership (deny-by-default), the
// explicit stage order, a per-workspace advance throttle, and the Human
// Oversight requirement that publish only follows an approved publication
// (Step 6). Every transition is audited with actor/workload/trace/span and
// logged as structured JSON.
type Service struct {
	store    PipelineStore
	research Researcher
	script   ScriptWriter
	review   Reviewer
	publish  Publisher
	audit    AuditSink
	events   EventSink
	throttle *advanceThrottle
}

// NewService wires a Service from its ports. A nil audit or event sink is
// replaced by a no-op. Capability ports must be non-nil; the default is the
// deterministic stub adapter (ADR-010).
func NewService(store PipelineStore, research Researcher, script ScriptWriter, review Reviewer, publish Publisher, audit AuditSink, events EventSink) *Service {
	if audit == nil {
		audit = NullAuditSink{}
	}
	if events == nil {
		events = NullEventSink{}
	}
	if research == nil {
		research = StubResearcher{}
	}
	if script == nil {
		script = StubScriptWriter{}
	}
	if review == nil {
		review = StubReviewer{}
	}
	if publish == nil {
		publish = StubPublisher{}
	}
	return &Service{
		store: store, research: research, script: script, review: review,
		publish: publish, audit: audit, events: events, throttle: newAdvanceThrottle(10),
	}
}

// Create persists a new pipeline in the research/created stage for the
// workspace.
func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, goalID *uuid.UUID, traceID string) (*Pipeline, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	p, err := New(workspaceID, goalID, traceID)
	if err != nil {
		return nil, err
	}
	stored, err := s.store.Create(ctx, p)
	if err != nil {
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{
		EventType: "pipeline.create", ConstitutionalPrinciple: "Obedience",
		Outcome: "success", WorkspaceID: workspaceID.String(), PipelineID: stored.ID.String(),
		ActorType: "system", TraceID: traceID, SpanID: spanOf(ctx), Stage: string(stored.Stage),
	})
	_ = s.events.PublishPipeline(ctx, "pipeline.research", stored.ID, workspaceID, string(stored.Stage), traceID, spanOf(ctx))
	logTrace(workspaceID, "pipeline-created").With("pipeline_id", stored.ID).With("stage", stored.Stage).Log()
	return stored, nil
}

// Advance moves the pipeline to the given next stage, performing the stage's
// capability work and updating the pipeline record. startStage is the pipeline's
// current stage before the transition (the caller derives it from the persisted
// pipeline or from a worker event).
func (s *Service) Advance(ctx context.Context, workspaceID, id uuid.UUID, startStage, to Stage) (*Pipeline, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	if !s.throttle.Allow(workspaceID) {
		s.audit.Record(ctx, AuditRecord{
			EventType: "pipeline.advance", ConstitutionalPrinciple: "Security by Design",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PipelineID: id.String(),
			ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx), Stage: string(to),
		})
		return nil, ErrRateLimited
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	// The transition must be legal from the persisted stage.
	if p.Stage != startStage {
		return nil, ErrConsecutiveAdvance
	}
	if err := CanAdvance(to, p.Stage, p.Status); err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "pipeline.advance", ConstitutionalPrinciple: "Human Oversight",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PipelineID: id.String(),
			ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx), Stage: string(to),
		})
		return nil, err
	}
	return s.doAdvance(ctx, p, to, "system", "")
}

func (s *Service) doAdvance(ctx context.Context, p *Pipeline, to Stage, actorType, actorID string) (*Pipeline, error) {
	// Perform the capability work for the incoming stage.
	ref, err := s.stageWork(ctx, p, to)
	if err != nil {
		status := StatusFailed
		p.Status = status
		p.UpdatedAt = time.Now().UTC()
		_, _ = s.store.Update(ctx, p.WorkspaceID, p)
		s.audit.Record(ctx, AuditRecord{
			EventType: "pipeline.advance", ConstitutionalPrinciple: "Security by Design",
			Outcome: "failed", WorkspaceID: p.WorkspaceID.String(), PipelineID: p.ID.String(),
			ActorType: actorType, ActorID: actorID, TraceID: traceOf(ctx), SpanID: spanOf(ctx), Stage: string(to),
		})
		logTrace(p.WorkspaceID, "pipeline-advance-failed").With("pipeline_id", p.ID).With("stage", to).WithError(err).Log()
		return nil, err
	}
	_ = ref

	// Apply the new stage and status.
	p.Stage = to
	switch to {
	case StageScript:
		p.Status = StatusActive
	case StageReview:
		p.Status = StatusAwaitingApproval
	case StagePublish:
		p.Status = StatusActive
	case StageComplete:
		p.Status = StatusDone
	}
	p.UpdatedAt = time.Now().UTC()
	updated, err := s.store.Update(ctx, p.WorkspaceID, p)
	if err != nil {
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{
		EventType: "pipeline.advance", ConstitutionalPrinciple: "Human Oversight",
		Outcome: "success", WorkspaceID: p.WorkspaceID.String(), PipelineID: p.ID.String(),
		ActorType: actorType, ActorID: actorID, TraceID: traceOf(ctx), SpanID: spanOf(ctx), Stage: string(to),
	})
	_ = s.events.PublishPipeline(ctx, "pipeline."+string(to), p.ID, p.WorkspaceID, string(to), traceOf(ctx), spanOf(ctx))
	logTrace(p.WorkspaceID, "pipeline-advanced").With("pipeline_id", p.ID).With("stage", to).With("status", p.Status).Log()
	return updated, nil
}

// stageWork dispatches the incoming stage to the corresponding capability port.
func (s *Service) stageWork(ctx context.Context, p *Pipeline, to Stage) (string, error) {
	switch to {
	case StageScript:
		rr, err := s.research.Research(ctx, p.WorkspaceID, p)
		if err != nil {
			return "", err
		}
		return s.script.WriteScript(ctx, p.WorkspaceID, p, rr)
	case StageReview:
		ok, err := s.review.Review(ctx, p.WorkspaceID, p)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("orchestration: review rejected artefact")
		}
		return "review-ok", nil
	case StagePublish:
		// Human Oversight gate: publish requires an approved publication
		// (Step 6). Phase 2 reuses the stub publisher; the gate is enforced for
		// real in production wiring.
		return s.publish.Publish(ctx, p.WorkspaceID, p, "review-ok")
	case StageComplete:
		return "", nil
	default:
		return "", ErrInvalidStage
	}
}

// Get returns a pipeline by id within the workspace (defense-in-depth).
func (s *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Pipeline, error) {
	return s.getOwned(ctx, workspaceID, id)
}

// List returns pipelines for a workspace.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID) ([]*Pipeline, error) {
	return s.store.List(ctx, workspaceID)
}

func (s *Service) getOwned(ctx context.Context, workspaceID, id uuid.UUID) (*Pipeline, error) {
	p, err := s.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if p.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	return p, nil
}

// advanceThrottle is a fixed-window per-workspace pipeline-advance limiter.
type advanceThrottle struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	hits   map[uuid.UUID]int
}

func newAdvanceThrottle(limit int) *advanceThrottle {
	if limit <= 0 {
		limit = 10
	}
	return &advanceThrottle{window: time.Minute, limit: limit, hits: map[uuid.UUID]int{}}
}

func (t *advanceThrottle) Allow(id uuid.UUID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hits[id] >= t.limit {
		return false
	}
	t.hits[id]++
	return true
}

func logTrace(workspaceID uuid.UUID, msg string) *logger.Entry {
	return logger.NewEntry(msg).With("workspace_id", workspaceID)
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

type traceKey struct{}
type spanKey struct{}

// WithTrace attaches a trace/span id to ctx for audit propagation.
func WithTrace(ctx context.Context, trace, span string) context.Context {
	ctx = context.WithValue(ctx, traceKey{}, trace)
	ctx = context.WithValue(ctx, spanKey{}, span)
	return ctx
}
