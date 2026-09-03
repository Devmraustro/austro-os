package task

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	logger "austro-os/internal/log"
)

// Service is the task application service. It enforces workspace ownership
// (deny-by-default), lifecycle transitions, and emits audit events and
// structured logs with optional trace/span propagation.
type Service struct {
	store TaskStore
	audit AuditSink
	events EventSink
}

// NewService wires a Service from its ports. A nil audit or event sink is
// replaced by a no-op so the service is always safe to construct.
func NewService(store TaskStore, audit AuditSink, events EventSink) *Service {
	if audit == nil {
		audit = NullAuditSink{}
	}
	if events == nil {
		events = NullEventSink{}
	}
	return &Service{store: store, audit: audit, events: events}
}

// Create persists a new backlog task for the workspace.
func (s *Service) Create(ctx context.Context, workspaceID uuid.UUID, title string, priority Priority, description string) (*Task, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	t, err := New(workspaceID, strings.TrimSpace(title), priority, description)
	if err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "task.create", ConstitutionalPrinciple: "Vision First",
			Outcome: "failed", WorkspaceID: workspaceID.String(), ActorType: "system",
		})
		return nil, err
	}
	id := t.ID
	stored, err := s.store.Create(ctx, t)
	if err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "task.create", ConstitutionalPrinciple: "Vision First",
			Outcome: "failed", WorkspaceID: workspaceID.String(), TaskID: id.String(), ActorType: "system",
		})
		return nil, err
	}
	t = stored
	s.audit.Record(ctx, AuditRecord{
		EventType: "task.create", ConstitutionalPrinciple: "Vision First",
		Outcome: "success", WorkspaceID: workspaceID.String(), TaskID: t.ID.String(), ActorType: "system",
	})
	_ = s.events.Publish(ctx, "task.created", t.ID, workspaceID, "", "")
	log(workspaceID, "task-created").With("task_id", t.ID).With("status", t.Status).Log()
	return t, nil
}

// Get returns a task by id within the workspace. The store enforces RLS first;
// an explicit workspace guard is kept as defense-in-depth so a task materialized
// for a different workspace is never handed to the caller.
func (s *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Task, error) {
	t, err := s.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if t.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	return t, nil
}

// List returns tasks in the workspace, optionally filtered by status.
func (s *Service) List(ctx context.Context, workspaceID uuid.UUID, status *Status) ([]*Task, error) {
	return s.store.List(ctx, workspaceID, status)
}

// Transition applies an explicit, validated lifecycle transition.
func (s *Service) Transition(ctx context.Context, workspaceID, id uuid.UUID, to Status) (*Task, error) {
	if !ValidStatus(to) {
		return nil, ErrStatus
	}
	t, err := s.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if t.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	if err := CanTransition(t.Status, to); err != nil {
		s.audit.Record(ctx, AuditRecord{
			EventType: "task.transition", ConstitutionalPrinciple: "Human Oversight",
			Outcome: "failed", WorkspaceID: workspaceID.String(), TaskID: id.String(),
			ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
		})
		return nil, err
	}
	prev := t.Status
	t.Status = to
	t, err = s.store.Update(ctx, workspaceID, t)
	if err != nil {
		return nil, err
	}
	rec := AuditRecord{
		EventType: "task.transition", ConstitutionalPrinciple: "Human Oversight",
		Outcome: "success", WorkspaceID: workspaceID.String(), TaskID: id.String(),
		ActorType: "system", TraceID: traceOf(ctx), SpanID: spanOf(ctx),
	}
	s.audit.Record(ctx, rec)
	_ = s.events.Publish(ctx, "task.transitioned", id, workspaceID, traceOf(ctx), spanOf(ctx))
	log(workspaceID, "task-transitioned").With("task_id", id).With("from", prev).With("to", to).Log()
	return t, nil
}

// Update applies non-lifecycle field updates (title, description, priority,
// assignee, deadline) after validation. Status changes must go through
// Transition.
func (s *Service) Update(ctx context.Context, workspaceID, id uuid.UUID, title string, priority Priority, description *string, assigneeType AssigneeType, assigneeID *uuid.UUID, deadline *time.Time) (*Task, error) {
	t, err := s.store.Get(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if t.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	if strings.TrimSpace(title) != "" {
		t.Title = strings.TrimSpace(title)
	}
	if priority != "" && !ValidPriority(priority) {
		return nil, ErrPriority
	}
	if priority != "" {
		t.Priority = priority
	}
	if description != nil {
		t.Description = *description
	}
	if assigneeType != "" && !ValidAssigneeType(assigneeType) {
		return nil, ErrAssigneeType
	}
	if assigneeType != "" {
		t.AssigneeType = assigneeType
	}
	if assigneeID != nil {
		t.AssigneeID = assigneeID
	}
	if deadline != nil {
		t.Deadline = deadline
	}
	t, err = s.store.Update(ctx, workspaceID, t)
	if err != nil {
		return nil, err
	}
	s.audit.Record(ctx, AuditRecord{
		EventType: "task.update", ConstitutionalPrinciple: "Quality Over Speed",
		Outcome: "success", WorkspaceID: workspaceID.String(), TaskID: id.String(), ActorType: "system",
	})
	return t, nil
}

// Delete removes a task within the workspace.
func (s *Service) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	return s.store.Delete(ctx, workspaceID, id)
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