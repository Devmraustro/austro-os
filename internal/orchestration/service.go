package orchestration

import (
	"context"
	"errors"
	"strings"
	"time"

	logger "austro-os/internal/log"
	"austro-os/internal/security"
	"github.com/google/uuid"
)

// Service is the Creator orchestration application service. It is the only
// place allowed to advance the aggregate and it never accepts a client-chosen
// destination from the HTTP surface.
type Service struct {
	store       PipelineStore
	research    Researcher
	script      ScriptWriter
	review      Reviewer
	publish     Publisher
	publication PublicationPort
	audit       AuditSink
	events      EventSink
	throttle    *security.Throttler
}

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
	return &Service{store: store, research: research, script: script, review: review, publish: publish, audit: audit, events: events, throttle: security.NewThrottler(time.Minute, 10)}
}

// SetPublicationPort attaches the narrow adapter to the existing Publishing
// service. It is called by composition; unit/domain callers may leave it nil
// and still exercise the state machine with the deterministic publisher.
func (s *Service) SetPublicationPort(port PublicationPort) *Service {
	s.publication = port
	return s
}

func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, goalID *uuid.UUID, traceID string) (*Pipeline, error) {
	return s.CreateWithIdempotency(ctx, workspaceID, goalID, traceID, "")
}

// CreateWithIdempotency makes a retry of the same authenticated request return
// the original aggregate when the persistent adapter supports the lookup.
func (s *Service) CreateWithIdempotency(ctx context.Context, workspaceID uuid.UUID, goalID *uuid.UUID, traceID, key string) (*Pipeline, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	key = strings.TrimSpace(key)
	if len(key) > 128 {
		return nil, ErrInvalidInput
	}
	if key != "" {
		if finder, ok := s.store.(interface {
			GetByIdempotency(context.Context, uuid.UUID, string) (*Pipeline, error)
		}); ok {
			if existing, err := finder.GetByIdempotency(ctx, workspaceID, key); err == nil {
				if existing.Stage == StageResearch && existing.Status == StatusCreated {
					if publishErr := s.events.PublishPipeline(ctx, "pipeline.research", existing.ID, workspaceID, string(existing.Stage), existing.TraceID, spanOf(ctx)); publishErr != nil {
						return existing, publishErr
					}
				}
				return existing, nil
			} else if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
	}
	p, err := NewWithIdempotency(workspaceID, goalID, traceID, key)
	if err != nil {
		return nil, err
	}
	stored, err := s.store.Create(ctx, p)
	if err != nil {
		if key != "" {
			if finder, ok := s.store.(interface {
				GetByIdempotency(context.Context, uuid.UUID, string) (*Pipeline, error)
			}); ok {
				if existing, lookupErr := finder.GetByIdempotency(ctx, workspaceID, key); lookupErr == nil {
					if existing.Stage == StageResearch && existing.Status == StatusCreated {
						if publishErr := s.events.PublishPipeline(ctx, "pipeline.research", existing.ID, workspaceID, string(existing.Stage), existing.TraceID, spanOf(ctx)); publishErr != nil {
							return existing, publishErr
						}
					}
					return existing, nil
				}
			}
		}
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{EventType: "pipeline.create", ConstitutionalPrinciple: "Obedience", Outcome: "success", WorkspaceID: workspaceID.String(), PipelineID: stored.ID.String(), ActorType: actorType(ctx, "system"), ActorID: actorID(ctx), TraceID: traceID, SpanID: spanOf(ctx), Stage: string(stored.Stage)})
	if err := s.events.PublishPipeline(ctx, "pipeline.research", stored.ID, workspaceID, string(stored.Stage), traceID, spanOf(ctx)); err != nil {
		return stored, err
	}
	logTrace(workspaceID, "pipeline-created").With("pipeline_id", stored.ID).With("stage", stored.Stage).Log()
	return stored, nil
}

// Advance is retained for internal callers and tests. The HTTP API does not
// expose it: workers derive the next stage from EventKind and approval uses the
// named Approve command below.
func (s *Service) Advance(ctx context.Context, workspaceID, id uuid.UUID, startStage, to Stage) (*Pipeline, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	if !s.throttle.Allow(workspaceID.String()) {
		return nil, ErrRateLimited
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if p.Stage != startStage {
		return nil, ErrConsecutiveAdvance
	}
	if err := CanAdvance(to, p.Stage, p.Status); err != nil {
		return nil, err
	}
	return s.doAdvance(ctx, p, to, "system", "")
}

// Approve is the only operation that can open the review → publish handoff.
// The actor is supplied by verified HTTP claims, never by the request body.
func (s *Service) Approve(ctx context.Context, workspaceID, id uuid.UUID, actor string) (*Pipeline, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, ErrUnauthorizedActor
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if p.Stage != StageReview || p.Status != StatusAwaitingApproval {
		return nil, ErrApprovalRequired
	}
	if s.publication != nil {
		if p.PublicationID == nil {
			return nil, ErrApprovalRequired
		}
		if err := s.publication.Approve(ctx, workspaceID, *p.PublicationID, actor); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	p.Status = StatusApproved
	p.ApprovedBy = actor
	p.ApprovedAt = &now
	p.FailureReason = ""
	p.UpdatedAt = now
	updated, err := s.store.Update(ctx, workspaceID, p)
	if err != nil {
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{EventType: "pipeline.approve", ConstitutionalPrinciple: "Human Oversight", Outcome: "success", WorkspaceID: workspaceID.String(), PipelineID: id.String(), ActorType: "human", ActorID: actor, TraceID: traceOf(ctx), SpanID: spanOf(ctx), Stage: string(StageReview)})
	if err := s.events.PublishPipeline(ctx, "pipeline.review_approved", id, workspaceID, string(StageReview), traceOf(ctx), spanOf(ctx)); err != nil {
		return updated, err
	}
	return updated, nil
}

// Retry resets only a failed aggregate to the exact failed stage's canonical
// predecessor state and re-enters the server-selected next step. There is no
// arbitrary stage argument.
func (s *Service) Retry(ctx context.Context, workspaceID, id uuid.UUID, actor string) (*Pipeline, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, ErrUnauthorizedActor
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if p.Status != StatusFailed {
		return nil, ErrInvalidTransition
	}
	if p.Stage == StageComplete {
		return nil, ErrTerminalState
	}
	switch p.Stage {
	case StageReview:
		if p.ApprovedBy != "" && p.ApprovedAt != nil {
			p.Status = StatusApproved
		} else {
			p.Status = StatusAwaitingApproval
		}
	case StageResearch:
		p.Status = StatusCreated
	default:
		p.Status = StatusActive
	}
	p.RetryCount++
	p.FailureReason = ""
	p.UpdatedAt = time.Now().UTC()
	if _, err := s.store.Update(ctx, workspaceID, p); err != nil {
		return nil, err
	}
	next, err := NextStage(p.Stage)
	if err != nil {
		return nil, err
	}
	return s.doAdvance(ctx, p, next, "human", actor)
}

func (s *Service) doAdvance(ctx context.Context, p *Pipeline, to Stage, actorType, actorID string) (*Pipeline, error) {
	ref, err := s.stageWork(ctx, p, to)
	if err != nil {
		p.Status = StatusFailed
		p.FailureReason = failureReason(err)
		p.UpdatedAt = time.Now().UTC()
		_, persistErr := s.store.Update(ctx, p.WorkspaceID, p)
		s.audit.Record(ctx, AuditRecord{EventType: "pipeline.advance", ConstitutionalPrinciple: "Security by Design", Outcome: "failed", WorkspaceID: p.WorkspaceID.String(), PipelineID: p.ID.String(), ActorType: actorType, ActorID: actorID, TraceID: traceOf(ctx), SpanID: spanOf(ctx), Stage: string(to)})
		if persistErr != nil {
			return nil, errors.Join(err, persistErr)
		}
		return nil, err
	}
	if to == StageReview && s.publication != nil {
		publicationID, prepErr := s.publication.Prepare(ctx, p.WorkspaceID, p)
		if prepErr != nil {
			p.Status = StatusFailed
			p.FailureReason = failureReason(prepErr)
			p.UpdatedAt = time.Now().UTC()
			_, _ = s.store.Update(ctx, p.WorkspaceID, p)
			return nil, prepErr
		}
		p.PublicationID = &publicationID
	}
	if to == StagePublish && s.publication != nil {
		if p.PublicationID == nil || !p.Approved() {
			return nil, ErrApprovalRequired
		}
		publishedRef, publishErr := s.publication.Publish(ctx, p.WorkspaceID, *p.PublicationID, p.ApprovedBy)
		if publishErr != nil {
			p.Status = StatusFailed
			p.FailureReason = failureReason(publishErr)
			p.UpdatedAt = time.Now().UTC()
			_, _ = s.store.Update(ctx, p.WorkspaceID, p)
			return nil, publishErr
		}
		p.PublishedReference = publishedRef
	}
	_ = ref
	p.Stage = to
	switch to {
	case StageScript, StagePublish:
		p.Status = StatusActive
	case StageReview:
		p.Status = StatusAwaitingApproval
	case StageComplete:
		p.Status = StatusDone
	}
	p.FailureReason = ""
	p.UpdatedAt = time.Now().UTC()
	updated, err := s.store.Update(ctx, p.WorkspaceID, p)
	if err != nil {
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{EventType: "pipeline.advance", ConstitutionalPrinciple: "Human Oversight", Outcome: "success", WorkspaceID: p.WorkspaceID.String(), PipelineID: p.ID.String(), ActorType: actorType, ActorID: actorID, TraceID: traceOf(ctx), SpanID: spanOf(ctx), Stage: string(to)})
	eventType := "pipeline." + string(to)
	if to == StageReview {
		eventType = "pipeline.review_ready"
	}
	if err := s.events.PublishPipeline(ctx, eventType, p.ID, p.WorkspaceID, string(to), traceOf(ctx), spanOf(ctx)); err != nil {
		return updated, err
	}
	logTrace(p.WorkspaceID, "pipeline-advanced").With("pipeline_id", p.ID).With("stage", to).With("status", p.Status).Log()
	return updated, nil
}

func (s *Service) stageWork(ctx context.Context, p *Pipeline, to Stage) (string, error) {
	switch to {
	case StageScript:
		rr, err := s.research.Research(ctx, p.WorkspaceID, p)
		if err != nil {
			return "", err
		}
		p.ResearchReference = boundReference(rr)
		sr, err := s.script.WriteScript(ctx, p.WorkspaceID, p, p.ResearchReference)
		if err != nil {
			return "", err
		}
		p.ScriptReference = boundReference(sr)
		return p.ScriptReference, nil
	case StageReview:
		ok, err := s.review.Review(ctx, p.WorkspaceID, p)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("orchestration: review rejected artefact")
		}
		p.ReviewReference = "review-ok"
		return p.ReviewReference, nil
	case StagePublish:
		if s.publication == nil {
			return s.publish.Publish(ctx, p.WorkspaceID, p, p.ReviewReference)
		}
		return "", nil
	case StageComplete:
		return "", nil
	default:
		return "", ErrInvalidStage
	}
}

func boundReference(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) > 512 {
		return ref[:512]
	}
	return ref
}

func failureReason(err error) string {
	if err == nil {
		return "stage_failed"
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 512 {
		return msg[:512]
	}
	if msg == "" {
		return "stage_failed"
	}
	return msg
}

func (s *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Pipeline, error) {
	return s.getOwned(ctx, workspaceID, id)
}
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID) ([]*Pipeline, error) {
	return s.store.List(ctx, workspaceID)
}

func (s *Service) getOwned(ctx context.Context, workspaceID, id uuid.UUID) (*Pipeline, error) {
	if workspaceID == uuid.Nil || id == uuid.Nil {
		return nil, ErrInvalidInput
	}
	p, err := s.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if p.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	return p, nil
}

func actorID(ctx context.Context) string {
	if v, ok := ctx.Value(actorKey{}).(string); ok {
		return v
	}
	return ""
}
func actorType(ctx context.Context, fallback string) string {
	if actorID(ctx) != "" {
		return "human"
	}
	return fallback
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
type actorKey struct{}

func WithTrace(ctx context.Context, trace, span string) context.Context {
	ctx = context.WithValue(ctx, traceKey{}, trace)
	return context.WithValue(ctx, spanKey{}, span)
}
func WithActor(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, actorKey{}, strings.TrimSpace(id))
}

func logTrace(workspaceID uuid.UUID, msg string) *logger.Entry {
	return logger.NewEntry(msg).With("workspace_id", workspaceID)
}
