package orchestration

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// Handler is the worker composition for the Creator workflow (ADR-010, ROADMAP
// §2.1). A Handler binds to a pipeline event and advances the pipeline to the
// next stage, so the orchestration runs as a deterministic message-driven loop
// compatible with the `internal/worker` RabbitMQ consumer and `internal/event`
// envelope. It is the only glue that connects an incoming event to
// `Service.Advance`; it never touches infrastructure directly (criterion #32).
type Handler struct {
	svc *Service
}

// NewHandler wraps a Service with the event-driven advance loop.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// AdvanceFromEvent consumes a pipeline event identified by its current stage
// and advances to the prescribed next stage, returning the updated pipeline.
// traceID/spanID are propagated onto the context for end-to-end observability
// (P13). The caller (a worker consumer) supplies the pipeline's current stage.
func (h *Handler) AdvanceFromEvent(ctx context.Context, workspaceID, pipelineID uuid.UUID, currentStage Stage, traceID, spanID string) (*Pipeline, error) {
	if traceID != "" {
		ctx = WithTrace(ctx, traceID, spanID)
	}
	next, err := NextStage(currentStage)
	if err != nil {
		return nil, err
	}
	return h.svc.Advance(ctx, workspaceID, pipelineID, currentStage, next)
}

// NextStage returns the canonical follower of a stage in the Creator workflow.
// A complete pipeline has no follower and surfaces ErrTerminalState.
func NextStage(stage Stage) (Stage, error) {
	switch stage {
	case StageResearch:
		return StageScript, nil
	case StageScript:
		return StageReview, nil
	case StageReview:
		return StagePublish, nil
	case StagePublish:
		return StageComplete, nil
	case StageComplete:
		return "", ErrTerminalState
	}
	return "", ErrInvalidStage
}

// EventKind maps a pipeline event string (e.g. "pipeline.research") to its
// stage and reports whether it is recognized.
func EventKind(typeName string) (Stage, bool) {
	v, ok := map[string]Stage{
		"pipeline.research": StageResearch,
		"pipeline.script":   StageScript,
		"pipeline.review":   StageReview,
		"pipeline.publish":  StagePublish,
		"pipeline.complete": StageComplete,
	}[strings.TrimSpace(typeName)]
	return v, ok
}
