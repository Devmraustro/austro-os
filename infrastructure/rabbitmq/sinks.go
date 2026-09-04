package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"austro-os/internal/ai"
	"austro-os/internal/event"
	logger "austro-os/internal/log"
	"austro-os/internal/orchestration"
	"austro-os/internal/publish"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Sink adapts internal domain events to the process event queue. It carries the
// trace/span correlation ids in both the envelope and the message headers so
// the worker consumer can propagate end-to-end observability (P13).
type Sink struct {
	channel *amqp.Channel
	queue   string
}

// NewSink returns a Sink publishing to queue on an existing channel (the
// caller owns the channel lifecycle; see Initialize).
func NewSink(channel *amqp.Channel, queue string) *Sink {
	if queue == "" {
		queue = "austro.events"
	}
	return &Sink{channel: channel, queue: queue}
}

// deliver marshals an envelope and publishes it durably with correlation headers.
func (s *Sink) deliver(env event.UniversalEnvelope, headers amqp.Table) error {
	if s.channel == nil {
		return fmt.Errorf("rabbitmq: sink has no channel")
	}
	body, err := env.MarshalJSON()
	if err != nil {
		return fmt.Errorf("rabbitmq: marshal event: %w", err)
	}
	return s.channel.Publish("", s.queue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Body:         body,
		MessageId:    env.EventID.String(),
		Headers:      headers,
	})
}

// baseEnvelope builds the envelope shared by the domain event sinks. The domain
// event name (e.g. "pipeline.script") is encoded in Details by each sink; the
// worker consumer decodes it there.
func baseEnvelope(targetType string, targetID, workspaceID uuid.UUID, traceID, spanID string, details map[string]string, hdrs amqp.Table) (event.UniversalEnvelope, amqp.Table) {
	now := time.Now().UTC()
	if hdrs == nil {
		hdrs = amqp.Table{}
	}
	raw, _ := json.Marshal(details)
	hdrs["trace_id"] = traceID
	hdrs["span_id"] = spanID
	hdrs["workspace_id"] = workspaceID.String()
	hdrs["target_type"] = targetType
	env := event.UniversalEnvelope{
		EventID:     uuid.New(),
		Timestamp:   now,
		WorkspaceID: workspaceID.String(),
		ActorType:   "system",
		TargetType:  targetType,
		TargetID:    targetID,
		EventType:   event.EventTypeCreated,
		Outcome:     "success",
		Details:     raw,
	}
	if tid, err := uuid.Parse(traceID); err == nil {
		env.TraceID = tid
	}
	if sid, err := uuid.Parse(spanID); err == nil {
		env.SpanID = sid
	}
	return env, hdrs
}

// PublishEventSink implements publish.EventSink over the queue.
type PublishEventSink struct {
	sink *Sink
}

// NewPublishEventSink returns the publishing event sink.
func NewPublishEventSink(sink *Sink) *PublishEventSink {
	return &PublishEventSink{sink: sink}
}

// Publish satisfies publish.EventSink.
func (s *PublishEventSink) Publish(_ context.Context, eventType string, publicationID, workspaceID uuid.UUID, traceID, spanID string) error {
	env, hdrs := baseEnvelope("publication", publicationID, workspaceID, traceID, spanID, map[string]string{
		"event": eventType,
	}, amqp.Table{"event_type": eventType})
	return s.sink.deliver(env, hdrs)
}

// PipelineEventSink implements orchestration.EventSink over the queue.
type PipelineEventSink struct {
	sink *Sink
}

// NewPipelineEventSink returns the pipeline event sink.
func NewPipelineEventSink(sink *Sink) *PipelineEventSink {
	return &PipelineEventSink{sink: sink}
}

// PublishPipeline satisfies orchestration.EventSink.
func (s *PipelineEventSink) PublishPipeline(_ context.Context, eventType string, pipelineID, workspaceID uuid.UUID, stage, traceID, spanID string) error {
	env, hdrs := baseEnvelope("pipeline", pipelineID, workspaceID, traceID, spanID, map[string]string{
		"event": eventType,
		"stage": stage,
	}, amqp.Table{"event_type": eventType})
	return s.sink.deliver(env, hdrs)
}

// DecisionSink implements ai.AuditSink over the queue: every AI gateway
// decision (including usage-limit rejections) is published for audit.
type DecisionSink struct {
	sink *Sink
}

// NewDecisionSink returns the AI decision sink.
func NewDecisionSink(sink *Sink) *DecisionSink {
	return &DecisionSink{sink: sink}
}

// Sink satisfies ai.AuditSink. The decision carries only non-secret metadata.
func (s *DecisionSink) Sink(decision ai.DecisionRecord) {
	env, hdrs := baseEnvelope("ai", uuid.Nil, uuid.Nil, decision.TraceID.String(), decision.SpanID.String(), map[string]string{
		"event":   decision.EventType,
		"outcome": decision.Outcome,
	}, amqp.Table{"event_type": decision.EventType, "principle": decision.ConstitutionalPrinciple})
	env.WorkspaceID = decision.WorkspaceID
	env.Outcome = decision.Outcome
	if err := s.sink.deliver(env, hdrs); err != nil {
		logger.NewEntry("ai-decision-publish-failed").SetLevel("error").WithError(err).Log()
	}
}

// PublishLogAuditSink implements publish.AuditSink with structured, redacted
// log output (minimal disclosure: no credentials or external references).
type PublishLogAuditSink struct{}

// NewPublishLogAuditSink returns the publishing audit log sink.
func NewPublishLogAuditSink() *PublishLogAuditSink {
	return &PublishLogAuditSink{}
}

// Record satisfies publish.AuditSink.
func (PublishLogAuditSink) Record(_ context.Context, rec publish.AuditRecord) {
	logger.NewEntry("publication-audit").
		With("event", rec.EventType).
		With("principle", rec.ConstitutionalPrinciple).
		With("outcome", rec.Outcome).
		With("workspace_id", rec.WorkspaceID).
		With("publication_id", rec.PublicationID).
		With("actor_type", rec.ActorType).
		With("trace_id", rec.TraceID).
		With("span_id", rec.SpanID).
		Log()
}

// PipelineLogAuditSink implements orchestration.AuditSink with structured,
// redacted log output.
type PipelineLogAuditSink struct{}

// NewPipelineLogAuditSink returns the orchestration audit log sink.
func NewPipelineLogAuditSink() *PipelineLogAuditSink {
	return &PipelineLogAuditSink{}
}

// Record satisfies orchestration.AuditSink.
func (PipelineLogAuditSink) Record(_ context.Context, rec orchestration.AuditRecord) {
	logger.NewEntry("pipeline-audit").
		With("event", rec.EventType).
		With("principle", rec.ConstitutionalPrinciple).
		With("outcome", rec.Outcome).
		With("workspace_id", rec.WorkspaceID).
		With("pipeline_id", rec.PipelineID).
		With("stage", rec.Stage).
		With("actor_type", rec.ActorType).
		With("trace_id", rec.TraceID).
		With("span_id", rec.SpanID).
		Log()
}
