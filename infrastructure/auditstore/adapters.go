package auditstore

import (
	"context"

	"austro-os/internal/audit"
	"austro-os/internal/memory"
	logger "austro-os/internal/log"
	"austro-os/internal/orchestration"
	"austro-os/internal/publish"

	"github.com/google/uuid"
)

// These adapters connect the two domain audit contracts (publish and
// orchestration) to the persistent chain writer.
//
// Both domain contracts are fire-and-forget: Record returns nothing. That is a
// real limitation, and it means a failure to persist a publishing or pipeline
// decision is logged loudly but cannot fail the operation the way an
// authentication failure does. Widening it means changing
// publish.AuditSink/orchestration.AuditSink to return an error, which touches
// every implementation and caller of those interfaces.

// PublishSink records publishing decisions (approval, rejection, publish
// transitions) in the persistent audit chain.
type PublishSink struct {
	store *Store
}

// NewPublishSink adapts the persistent store to publish.AuditSink.
func NewPublishSink(store *Store) *PublishSink { return &PublishSink{store: store} }

var _ publish.AuditSink = (*PublishSink)(nil)

// Record persists one publishing decision.
func (s *PublishSink) Record(ctx context.Context, rec publish.AuditRecord) {
	_, err := s.store.Append(ctx, audit.Record{
		EventType:   rec.EventType,
		ActorType:   rec.ActorType,
		ActorID:     parseID(rec.ActorID),
		TargetType:  "publication",
		TargetID:    parseID(rec.PublicationID),
		Outcome:     rec.Outcome,
		Principle:   rec.ConstitutionalPrinciple,
		WorkspaceID: workspacePtr(rec.WorkspaceID),
		TraceID:     parseID(rec.TraceID),
		SpanID:      parseID(rec.SpanID),
	})
	if err != nil {
		logger.NewEntry("audit-publish-persist-failed").SetLevel("error").
			With("event_type", rec.EventType).WithError(err).Log()
	}
}

// PipelineSink records orchestration decisions (pipeline stage and status
// transitions) in the persistent audit chain.
type PipelineSink struct {
	store *Store
}

// NewPipelineSink adapts the persistent store to orchestration.AuditSink.
func NewPipelineSink(store *Store) *PipelineSink { return &PipelineSink{store: store} }

var _ orchestration.AuditSink = (*PipelineSink)(nil)

// Record persists one orchestration decision.
func (s *PipelineSink) Record(ctx context.Context, rec orchestration.AuditRecord) {
	_, err := s.store.Append(ctx, audit.Record{
		EventType:   rec.EventType,
		ActorType:   rec.ActorType,
		ActorID:     parseID(rec.ActorID),
		TargetType:  "pipeline",
		TargetID:    parseID(rec.PipelineID),
		Outcome:     rec.Outcome,
		Principle:   rec.ConstitutionalPrinciple,
		WorkspaceID: workspacePtr(rec.WorkspaceID),
		TraceID:     parseID(rec.TraceID),
		SpanID:      parseID(rec.SpanID),
		Details:     map[string]any{"stage": rec.Stage},
	})
	if err != nil {
		logger.NewEntry("audit-pipeline-persist-failed").SetLevel("error").
			With("event_type", rec.EventType).WithError(err).Log()
	}
}

func parseID(s string) uuid.UUID {
	if s == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func workspacePtr(s string) *uuid.UUID {
	id := parseID(s)
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// MemorySink adapts the workspace-scoped memory audit contract to the
// persistent append-only chain. The memory package keeps its domain port small;
// this adapter is the production boundary that supplies actor, workspace,
// correlation and durable failure semantics.
type MemorySink struct {
	store *Store
}

// NewMemorySink returns a persistent memory audit adapter.
func NewMemorySink(store *Store) *MemorySink { return &MemorySink{store: store} }

var _ memory.ErrorAuditSink = (*MemorySink)(nil)

// Record preserves the existing fire-and-forget domain interface for callers
// that do not need to surface an audit failure. HTTP memory mutations use
// RecordError through the optional stronger interface below.
func (s *MemorySink) Record(ctx context.Context, rec memory.AuditRecord) {
	if err := s.RecordError(ctx, rec); err != nil {
		logger.NewEntry("audit-memory-persist-failed").SetLevel("error").
			With("event_type", rec.EventType).WithError(err).Log()
	}
}

// RecordError persists one memory decision and returns failures to the Bank so
// the HTTP write path can fail closed instead of reporting an unaudited success.
func (s *MemorySink) RecordError(ctx context.Context, rec memory.AuditRecord) error {
	returnError := func(err error) error {
		if err != nil {
			logger.NewEntry("audit-memory-persist-failed").SetLevel("error").
				With("event_type", rec.EventType).WithError(err).Log()
		}
		return err
	}

	var actorID uuid.UUID
	if rec.ActorID != "" {
		parsed, err := uuid.Parse(rec.ActorID)
		if err != nil {
			return returnError(err)
		}
		actorID = parsed
	}
	return returnError(func() error {
		var workspaceID *uuid.UUID
		if rec.WorkspaceID != "" {
			parsed, err := uuid.Parse(rec.WorkspaceID)
			if err != nil {
				return err
			}
			workspaceID = &parsed
		}
		var traceID, spanID uuid.UUID
		if rec.TraceID != "" {
			parsed, err := uuid.Parse(rec.TraceID)
			if err != nil {
				return err
			}
			traceID = parsed
		}
		if rec.SpanID != "" {
			parsed, err := uuid.Parse(rec.SpanID)
			if err != nil {
				return err
			}
			spanID = parsed
		}
		_, err := s.store.Append(ctx, audit.Record{
			EventType:   rec.EventType,
			ActorType:   rec.ActorType,
			ActorID:     actorID,
			TargetType:  "memory_cell",
			TargetID:    uuid.Nil,
			Outcome:     rec.Outcome,
			Principle:   rec.ConstitutionalPrinciple,
			WorkspaceID: workspaceID,
			TraceID:     traceID,
			SpanID:      spanID,
			Details: map[string]any{
				"layer": rec.Layer,
				"key":   rec.Key,
			},
		})
		return err
	}())
}
