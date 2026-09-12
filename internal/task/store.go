package task

import (
	"context"

	"github.com/google/uuid"
)

// TaskStore is the workspace-scoped persistence port for tasks. Implementations
// must enforce RLS/workspace isolation so that reads and writes never cross
// workspace boundaries. It is implemented by an infrastructure adapter; the
// domain never depends on the adapter.
type TaskStore interface {
	// Create persists a new task and returns it.
	Create(ctx context.Context, t *Task) (*Task, error)
	// Get returns a task by id within the given workspace. Cross-workspace
	// reads must not succeed (RLS + explicit workspace guard).
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Task, error)
	// List returns tasks in a workspace, optionally filtered by status.
	List(ctx context.Context, workspaceID uuid.UUID, status *Status) ([]*Task, error)
	// Update persists field changes for a task owned by the workspace.
	Update(ctx context.Context, workspaceID uuid.UUID, t *Task) (*Task, error)
	// Delete removes a task within a workspace (soft-delete optional).
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

// AuditSink is the audit-compatible decision sink used by the task service.
// It mirrors the shape of the Phase 1 audit decision without coupling the
// domain to the hash-chain implementation. A concrete adapter connects this to
// the audit chain at the composition boundary.
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord)
}

// AuditRecord is the task service's audit contract. It carries only non-secret
// metadata for an append-only, principle-tagged decision.
type AuditRecord struct {
	EventType               string
	ConstitutionalPrinciple string
	Outcome                 string
	WorkspaceID             string
	TaskID                  string
	ActorType               string
	ActorID                 string
	TraceID                 string
	SpanID                  string
}

// NullAuditSink discards decisions (default when none is injected).
type NullAuditSink struct{}

// Record implements AuditSink by discarding the decision.
func (NullAuditSink) Record(_ context.Context, _ AuditRecord) {}

// EventSink publishes domain events (e.g. task.created, task.transitioned) to
// an event bus for downstream consumers. Domain stays behind the port.
type EventSink interface {
	Publish(ctx context.Context, eventType string, taskID uuid.UUID, workspaceID uuid.UUID, traceID, spanID string) error
}

// NullEventSink is a no-op publisher.
type NullEventSink struct{}

// Publish implements EventSink as a no-op.
func (NullEventSink) Publish(_ context.Context, _ string, _ uuid.UUID, _ uuid.UUID, _, _ string) error {
	return nil
}
