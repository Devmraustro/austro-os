package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"austro-os/internal/event"
	"austro-os/internal/execution"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

// ExecutionQueue is the dedicated AI execution dispatch queue. Claimed retries
// are delivered here, separate from the pipeline/publication events queue, so
// the event consumer never receives (and rejects) an execution payload. It is
// declared durably by the sink's reconnector and by the worker consumer.
const ExecutionQueue = "austro.executions"

// ExecutionPublisher implements execution.Publisher over the supervised sink:
// each fenced dispatch is wrapped in a UniversalEnvelope (TargetType
// "execution") whose Details carry the exact PublishMessage, and published
// durably. The message is at-least-once — RecordDispatch fences redundant
// publishes — exactly as the dispatcher contract requires.
type ExecutionPublisher struct {
	sink *Sink
}

// NewExecutionPublisher returns the execution dispatch publisher. The sink must
// publish to ExecutionQueue for the worker consumer to see the messages.
func NewExecutionPublisher(sink *Sink) *ExecutionPublisher {
	return &ExecutionPublisher{sink: sink}
}

// Publish delivers one claimed retry to the execution queue.
func (p *ExecutionPublisher) Publish(ctx context.Context, msg execution.PublishMessage) error {
	env, hdrs, err := executionEnvelope(msg)
	if err != nil {
		return err
	}
	return p.sink.deliver(env, hdrs)
}

// executionEnvelope builds the dispatch envelope and headers for a claimed
// retry. Details carry the exact PublishMessage JSON so the consumer decodes
// the fenced identity losslessly (see ExecutionPublisher round-trip tests).
func executionEnvelope(msg execution.PublishMessage) (event.UniversalEnvelope, amqp.Table, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return event.UniversalEnvelope{}, nil, fmt.Errorf("rabbitmq: marshal execution dispatch: %w", err)
	}
	env := event.UniversalEnvelope{
		EventID:     uuid.New(),
		Timestamp:   time.Now().UTC(),
		WorkspaceID: msg.WorkspaceID.String(),
		ActorType:   "system",
		TargetType:  "execution",
		TargetID:    msg.ExecutionID,
		EventType:   event.EventTypeCreated,
		Outcome:     "dispatched",
		Details:     raw,
	}
	hdrs := amqp.Table{
		"event_type":   "execution.dispatch",
		"workspace_id": msg.WorkspaceID.String(),
		"execution_id": msg.ExecutionID.String(),
		"retry_id":     msg.RetryID.String(),
		"generation":   msg.Generation,
	}
	return env, hdrs, nil
}
