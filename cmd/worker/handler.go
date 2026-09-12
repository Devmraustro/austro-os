package main

import (
	"context"
	"encoding/json"
	"errors"

	"austro-os/internal/composition"
	"austro-os/internal/event"
	logger "austro-os/internal/log"
	"austro-os/internal/orchestration"
	"austro-os/internal/worker"

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

		// Everything below this point is a structural defect in the message
		// itself. None of it can succeed on a retry, so each is reported as
		// permanent: requeueing would redeliver it forever and, with the
		// consumer's prefetch of 1, block every message behind it.
		var d pipelineEventDetails
		if err := json.Unmarshal(env.Details, &d); err != nil {
			return worker.PermanentErrorf("decode pipeline event details: %v", err)
		}
		stage := orchestration.Stage(d.Stage)
		if !orchestration.ValidStage(stage) {
			return worker.PermanentErrorf("pipeline event carries unknown stage %q", d.Stage)
		}
		workspaceID, err := uuid.Parse(env.WorkspaceID)
		if err != nil {
			return worker.PermanentErrorf("pipeline event carries invalid workspace id: %v", err)
		}
		if env.TargetID == uuid.Nil {
			return worker.PermanentErrorf("pipeline event missing pipeline id")
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
			// Advancement failed against a live dependency, so it may well
			// succeed on redelivery; leave it retryable.
			logger.NewEntry("worker-pipeline-advance-error").
				SetLevel("error").
				With("pipeline_id", env.TargetID).
				With("stage", string(stage)).
				WithError(err).
				Log()
			return err
		}
		return nil
	}
}
