package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"austro-os/internal/composition"
	"austro-os/internal/event"
	logger "austro-os/internal/log"
	"austro-os/internal/orchestration"

	"github.com/google/uuid"
)

// pipelineEventDetails is the payload encoded by the RabbitMQ pipeline event
// sink (infrastructure/rabbitmq): the domain event name and the pipeline stage.
type pipelineEventDetails struct {
	Event string `json:"event"`
	Stage string `json:"stage"`
}

// newHandler returns the worker event handler for the composed runtime. It
// advances creator pipelines from pipeline.* events (the message-driven
// orchestration loop, ADR-010) and acknowledges everything else; publication
// events are processed synchronously by the publishing service, so the worker
// only observes them.
func newHandler(rt *composition.Runtime) func(*event.UniversalEnvelope) error {
	return func(env *event.UniversalEnvelope) error {
		if env.TargetType != "pipeline" {
			logger.NewEntry("worker-event-ack").
				With("event_type", string(env.EventType)).
				With("target_type", env.TargetType).
				Log()
			return nil
		}

		var d pipelineEventDetails
		if err := json.Unmarshal(env.Details, &d); err != nil {
			return fmt.Errorf("decode pipeline event details: %w", err)
		}
		stage := orchestration.Stage(d.Stage)
		if !orchestration.ValidStage(stage) {
			return fmt.Errorf("pipeline event carries unknown stage %q", d.Stage)
		}
		workspaceID, err := uuid.Parse(env.WorkspaceID)
		if err != nil {
			return fmt.Errorf("pipeline event carries invalid workspace id: %w", err)
		}
		if env.TargetID == uuid.Nil {
			return fmt.Errorf("pipeline event missing pipeline id")
		}

		_, err = rt.Handler.AdvanceFromEvent(
			context.Background(),
			workspaceID,
			env.TargetID,
			stage,
			env.TraceID.String(),
			env.SpanID.String(),
		)
		if errors.Is(err, orchestration.ErrTerminalState) {
			// A complete pipeline has no follower; acknowledge and stop.
			logger.NewEntry("worker-pipeline-complete").With("pipeline_id", env.TargetID).Log()
			return nil
		}
		if err != nil {
			logger.NewEntry("worker-pipeline-advance-error").
				With("pipeline_id", env.TargetID).
				With("stage", string(stage)).
				WithError(err).
				Log()
			return err
		}
		return nil
	}
}
