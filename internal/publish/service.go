package publish

import (
	"context"
	"strings"
	"time"

	logger "austro-os/internal/log"
	"austro-os/internal/security"
	"github.com/google/uuid"
)

// Service is the publishing application service. It enforces workspace
// ownership (deny-by-default), the explicit reviewed lifecycle, mandatory human
// approval before publish, a per-workspace publish throttle, and emits
// principle-tagged audit and structured JSON logs with trace/span propagation.
type Service struct {
	store    PublicationStore
	pub      Publisher
	audit    AuditSink
	events   EventSink
	throttle *security.Throttler
}

// NewService wires a Service from its ports. A nil audit or event sink is
// replaced by a no-op. The Publisher is required and must be the deterministic
// stub in Phase 2 (never nil).
func NewService(store PublicationStore, pub Publisher, audit AuditSink, events EventSink) *Service {
	if audit == nil {
		audit = NullAuditSink{}
	}
	if events == nil {
		events = NullEventSink{}
	}
	if pub == nil {
		pub = StubPublisher{}
	}
	return &Service{store: store, pub: pub, audit: audit, events: events, throttle: security.NewThrottler(time.Minute, 10)}
}

// Create persists a new queued publication for the workspace. The no-key form
// remains useful to domain callers; HTTP callers should use CreateWithIdempotency.
func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, goalID, taskID *uuid.UUID, title, body, platform string) (*Publication, error) {
	return s.create(ctx, workspaceID, goalID, taskID, title, body, platform, "")
}

// CreateWithIdempotency persists a publication under a caller-supplied key.
// When the concrete store supports lookup, a repeated key returns the original
// aggregate instead of creating a second publication. The database also has a
// unique workspace/key constraint as a final race-safe backstop.
func (s *Service) CreateWithIdempotency(ctx context.Context, workspaceID uuid.UUID, goalID, taskID *uuid.UUID, title, body, platform, key string) (*Publication, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return s.Create(ctx, workspaceID, goalID, taskID, title, body, platform)
	}
	if len(key) > 128 {
		return nil, ErrInvalidInput
	}
	if finder, ok := s.store.(interface {
		GetByIdempotency(context.Context, uuid.UUID, string) (*Publication, error)
	}); ok {
		if existing, err := finder.GetByIdempotency(ctx, workspaceID, key); err == nil {
			return existing, nil
		} else if err != ErrNotFound {
			return nil, err
		}
	}
	return s.create(ctx, workspaceID, goalID, taskID, title, body, platform, key)
}

func (s *Service) create(ctx context.Context, workspaceID uuid.UUID, goalID, taskID *uuid.UUID, title, body, platform, idempotencyKey string) (*Publication, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	p, err := New(workspaceID, goalID, taskID, title, body, platform)
	if err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.create", ConstitutionalPrinciple: "Vision First",
			Outcome: "failed", WorkspaceID: workspaceID.String(), ActorType: actorType(ctx, "system"), ActorID: actorID(ctx),
		})
		return nil, err
	}
	p.IdempotencyKey = idempotencyKey
	id := p.ID
	stored, err := s.store.Create(ctx, p)
	if err != nil {
		// A concurrent request may have won the unique key race. Resolve it to
		// the already-created aggregate rather than leaking a database error or
		// creating a second delivery record.
		if idempotencyKey != "" {
			if finder, ok := s.store.(interface {
				GetByIdempotency(context.Context, uuid.UUID, string) (*Publication, error)
			}); ok {
				if existing, lookupErr := finder.GetByIdempotency(ctx, workspaceID, idempotencyKey); lookupErr == nil {
					return existing, nil
				}
			}
		}
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.create", ConstitutionalPrinciple: "Vision First",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(), ActorType: actorType(ctx, "system"), ActorID: actorID(ctx),
		})
		return nil, err
	}
	p = stored
	s.audit.Record(ctx, AuditRecord{
		EventType: "publication.create", ConstitutionalPrinciple: "Vision First",
		Outcome: "success", WorkspaceID: workspaceID.String(), PublicationID: p.ID.String(), ActorType: actorType(ctx, "system"), ActorID: actorID(ctx),
	})
	_ = s.events.Publish(ctx, "publication.created", p.ID, workspaceID, traceOf(ctx), spanOf(ctx))
	log(workspaceID, "publication-created").With("publication_id", p.ID).With("status", p.Status).Log()
	return p, nil
}

// ToReview moves a queued publication into review.
func (s *Service) ToReview(ctx context.Context, workspaceID, id uuid.UUID) (*Publication, error) {
	return s.transition(ctx, workspaceID, id, StatusReview, actorType(ctx, "human"), actorID(ctx))
}

// Approve records mandatory human approval (review → approved). A publication
// cannot reach Publish without this.
func (s *Service) Approve(ctx context.Context, workspaceID, id uuid.UUID, humanActor string) (*Publication, error) {
	if strings.TrimSpace(humanActor) == "" {
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.approve", ConstitutionalPrinciple: "Human Oversight",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(),
		})
		return nil, ErrUnauthorizedApprover
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if p.Status != StatusReview {
		return nil, ErrInvalidTransition
	}
	now := time.Now().UTC()
	p.ApprovedBy = &humanActor
	p.ApprovedAt = &now
	return s.apply(ctx, workspaceID, p, StatusApproved, "human", humanActor)
}

// Reject records a human rejection (review → rejected). Terminal; no publish.
func (s *Service) Reject(ctx context.Context, workspaceID, id uuid.UUID, humanActor string) (*Publication, error) {
	if strings.TrimSpace(humanActor) == "" {
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.reject", ConstitutionalPrinciple: "Human Oversight",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(),
		})
		return nil, ErrUnauthorizedRejecter
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if p.Status != StatusReview {
		return nil, ErrInvalidTransition
	}
	now := time.Now().UTC()
	p.RejectedBy = &humanActor
	p.RejectedAt = &now
	return s.apply(ctx, workspaceID, p, StatusRejected, "human", humanActor)
}

// Publish delivers an approved publication to the platform. It requires that
// the publication has already been human-approved (ErrApprovalRequired
// otherwise) and that the releasing actor is a human. No publish without
// approval is guaranteed here.
func (s *Service) Publish(ctx context.Context, workspaceID, id uuid.UUID, humanActor string) (*Publication, error) {
	if strings.TrimSpace(humanActor) == "" {
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.publish", ConstitutionalPrinciple: "Human Oversight",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(),
		})
		return nil, ErrUnauthorizedPublisher
	}
	if !s.throttle.Allow(workspaceID.String()) {
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.publish", ConstitutionalPrinciple: "Security by Design",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(),
		})
		return nil, ErrRateLimited
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	// Validate the state-machine transition first: only approved publications
	// may move to published (review → approved → published). Any other source
	// state is an invalid transition, reported before the approval gate.
	if err := CanTransition(p.Status, StatusPublished); err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.publish", ConstitutionalPrinciple: "Human Oversight",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(),
			ActorType: "human", ActorID: humanActor, TraceID: traceOf(ctx), SpanID: spanOf(ctx),
		})
		return nil, err
	}
	if !p.HasApproval() {
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.publish", ConstitutionalPrinciple: "Human Oversight",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(),
		})
		return nil, ErrApprovalRequired
	}
	ref, err := s.pub.Publish(ctx, p)
	if err != nil {
		// Delivery failure is a domain result, not a successful acknowledgement.
		// Persist it before returning the adapter error so operators and the UI
		// can distinguish approved from failed delivery after a restart.
		p.FailureReason = failureReason(err)
		failed, persistErr := s.apply(ctx, workspaceID, p, StatusFailed, "human", humanActor)
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.publish", ConstitutionalPrinciple: "Security by Design",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(),
			ActorType: "human", ActorID: humanActor, TraceID: traceOf(ctx), SpanID: spanOf(ctx),
		})
		if persistErr != nil {
			return nil, persistErr
		}
		return failed, err
	}
	now := time.Now().UTC()
	p.PublishedAt = &now
	p.PublishedBy = &humanActor
	p.ExternalReference = ref
	updated, err := s.apply(ctx, workspaceID, p, StatusPublished, "human", humanActor)
	if updated != nil {
		// Minimal disclosure: never emit a raw external/platform reference;
		// emit an irreversible digest on the record and a redacted value in logs.
		digest, _ := security.NewExternalIDHasher([]byte("publish")).Hash(ref)
		log(workspaceID, "publication-delivered").With("publication_id", updated.ID).
			With("platform", updated.Platform).With("ref", security.RedactSecret(ref)).With("ref_digest", digest).Log()
	}
	return updated, err
}

// Retry re-attempts a failed delivery while preserving the recorded human
// approval. It is the only recovery path out of FAILED; callers cannot turn it
// into an arbitrary status update. A failed retry remains FAILED with a new,
// bounded failure reason.
func (s *Service) Retry(ctx context.Context, workspaceID, id uuid.UUID, humanActor string) (*Publication, error) {
	if strings.TrimSpace(humanActor) == "" {
		return nil, ErrUnauthorizedPublisher
	}
	if !s.throttle.Allow(workspaceID.String()) {
		return nil, ErrRateLimited
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil { return nil, err }
	if p.Status != StatusFailed { return nil, ErrInvalidTransition }
	if p.ApprovedBy == nil || p.ApprovedAt == nil { return nil, ErrApprovalRequired }

	// Publisher adapters intentionally accept only approved aggregates. Retry
	// keeps FAILED durable in the store but uses the approved state for this
	// in-memory delivery attempt; no client can observe or set this state.
	p.Status = StatusApproved
	ref, err := s.pub.Publish(ctx, p)
	if err != nil {
		p.Status = StatusFailed
		p.FailureReason = failureReason(err)
		p.UpdatedAt = time.Now().UTC()
		failed, persistErr := s.store.Update(ctx, workspaceID, p)
		s.audit.Record(ctx, AuditRecord{EventType: "publication.retry", ConstitutionalPrinciple: "Human Oversight", Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: id.String(), ActorType: "human", ActorID: humanActor, TraceID: traceOf(ctx), SpanID: spanOf(ctx)})
		if persistErr != nil { return nil, persistErr }
		return failed, err
	}
	p.FailureReason = ""
	p.ExternalReference = ref
	now := time.Now().UTC()
	p.PublishedAt = &now
	p.PublishedBy = &humanActor
	return s.apply(ctx, workspaceID, p, StatusPublished, "human", humanActor)
}

// Cancel cancels a queued or in-review publication.
func (s *Service) Cancel(ctx context.Context, workspaceID, id uuid.UUID) (*Publication, error) {
	return s.transition(ctx, workspaceID, id, StatusCancelled, "system", "")
}

// Get returns a publication by id within the workspace (defense-in-depth).
func (s *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Publication, error) {
	return s.getOwned(ctx, workspaceID, id)
}

// List returns publications in the workspace, optionally filtered by status.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID, status *Status) ([]*Publication, error) {
	return s.store.List(ctx, workspaceID, status)
}

func (s *Service) transition(ctx context.Context, workspaceID, id uuid.UUID, to Status, actorType, actorID string) (*Publication, error) {
	if !ValidStatus(to) {
		return nil, ErrInvalidStatus
	}
	p, err := s.getOwned(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	return s.apply(ctx, workspaceID, p, to, actorType, actorID)
}

// apply mutates a loaded publication to `to`, persisting through the store and
// auditing the transition.
func (s *Service) apply(ctx context.Context, workspaceID uuid.UUID, p *Publication, to Status, actorType, actorID string) (*Publication, error) {
	if err := CanTransition(p.Status, to); err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "publication.transition", ConstitutionalPrinciple: "Human Oversight",
			Outcome: "failed", WorkspaceID: workspaceID.String(), PublicationID: p.ID.String(),
			ActorType: actorType, ActorID: actorID, TraceID: traceOf(ctx), SpanID: spanOf(ctx),
		})
		return nil, err
	}
	p.Status = to
	p.UpdatedAt = time.Now().UTC()
	updated, err := s.store.Update(ctx, workspaceID, p)
	if err != nil {
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{
		EventType: "publication.transition", ConstitutionalPrinciple: "Human Oversight",
		Outcome: "success", WorkspaceID: workspaceID.String(), PublicationID: p.ID.String(),
		ActorType: actorType, ActorID: actorID, TraceID: traceOf(ctx), SpanID: spanOf(ctx),
	})
	_ = s.events.Publish(ctx, "publication."+string(to), p.ID, workspaceID, traceOf(ctx), spanOf(ctx))
	log(workspaceID, "publication-transitioned").With("publication_id", p.ID).With("to", to).With("actor", actorID).Log()
	return updated, nil
}

func (s *Service) getOwned(ctx context.Context, workspaceID, id uuid.UUID) (*Publication, error) {
	p, err := s.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if p.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	return p, nil
}

// HasApproval reports whether a recorded human approval is present.
func (p *Publication) HasApproval() bool {
	return p.Status == StatusApproved && p.ApprovedBy != nil && p.ApprovedAt != nil
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

// WithActor attaches the verified caller to a domain context. The handler is
// the only layer allowed to obtain this value from a token.
func WithActor(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, actorKey{}, strings.TrimSpace(id))
}

type actorKey struct{}

func failureReason(err error) string {
	if err == nil {
		return "delivery_failed"
	}
	reason := strings.TrimSpace(err.Error())
	if reason == "" {
		return "delivery_failed"
	}
	if len(reason) > 512 {
		return reason[:512]
	}
	return reason
}

func log(workspaceID uuid.UUID, msg string) *logger.Entry {
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

// WithTrace attaches a trace id to ctx for audit propagation.
func WithTrace(ctx context.Context, trace, span string) context.Context {
	ctx = context.WithValue(ctx, traceKey{}, trace)
	ctx = context.WithValue(ctx, spanKey{}, span)
	return ctx
}
