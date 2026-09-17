package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

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

type publicationEventDetails struct {
	Event string `json:"event"`
}

// newHandler returns the worker event handler for the composed runtime. It
// advances creator pipelines from pipeline.* events (the message-driven
// orchestration loop, ADR-010) and acknowledges everything else; publication
// events are processed synchronously by the publishing service; the worker
// validates and observes their durable lifecycle messages without redelivering.
func newHandler(rt *composition.Runtime) func(*event.UniversalEnvelope) error {
	return func(env *event.UniversalEnvelope) error {
		if env.TargetType == "publication" {
			// Publishing delivery is synchronous in the existing publish.Service:
			// this queue message is the durable lifecycle observation emitted only
			// after the state write. The worker must nevertheless parse and validate
			// publication messages instead of blindly acknowledging every unknown
			// target; malformed or unscoped messages are permanent failures.
			var d publicationEventDetails
			if err := json.Unmarshal(env.Details, &d); err != nil {
				return worker.PermanentErrorf("decode publication event details: %v", err)
			}
			if !strings.HasPrefix(d.Event, "publication.") {
				return worker.PermanentErrorf("publication event carries invalid event %q", d.Event)
			}
			if _, err := uuid.Parse(env.WorkspaceID); err != nil || env.TargetID == uuid.Nil {
				return worker.PermanentErrorf("publication event is missing a valid workspace or publication id")
			}
			logger.NewEntry("worker-publication-observed").
				With("event", d.Event).With("publication_id", env.TargetID).
				With("outcome", env.Outcome).Log()
			return nil
		}
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
		eventStage, recognized := orchestration.EventKind(d.Event)
		if !recognized || eventStage != stage {
			return worker.PermanentErrorf("pipeline event name %q does not match stage %q", d.Event, d.Stage)
		}
		workspaceID, err := uuid.Parse(env.WorkspaceID)
		if err != nil {
			return worker.PermanentErrorf("pipeline event carries invalid workspace id: %v", err)
		}
		if env.TargetID == uuid.Nil {
			return worker.PermanentErrorf("pipeline event missing pipeline id")
		}
		// Review readiness is deliberately an acknowledgement-only notification.
		// It cannot advance a pipeline past the human gate. Complete is likewise
		// a terminal observation, not another command.
		if d.Event == "pipeline.review" || d.Event == "pipeline.review_ready" || d.Event == "pipeline.complete" {
			return nil
		}

		_, err = rt.Handler.AdvanceFromEvent(
			context.Background(),
			workspaceID,
			env.TargetID,
			stage,
			env.TraceID.String(),
			env.SpanID.String(),
		)
		if err != nil {
			// A redelivered event is harmless after its stage has already been
			// durably advanced. A failed aggregate, however, stays retryable and
			// must not be silently acknowledged.
			current, lookupErr := rt.Orchestration.Get(context.Background(), workspaceID, env.TargetID)
			if lookupErr == nil && current.Status == orchestration.StatusDone {
				return nil
			}
			if lookupErr == nil && current.Stage != stage && orchestration.IsAfter(current.Stage, stage) {
				return nil
			}
			if errors.Is(err, orchestration.ErrTerminalState) && lookupErr == nil && current.Status != orchestration.StatusFailed {
				return nil
			}
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
