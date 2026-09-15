package orchestration

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// AdvanceFromEvent consumes one validated worker event. The review_ready and
// complete notifications are observations, not commands; approval is the
// explicit command that produces the review_approved event.
func (h *Handler) AdvanceFromEvent(ctx context.Context, workspaceID, pipelineID uuid.UUID, currentStage Stage, traceID, spanID string) (*Pipeline, error) {
	if traceID != "" {
		ctx = WithTrace(ctx, traceID, spanID)
	}
	next, err := NextStage(currentStage)
	if err != nil {
		return nil, err
	}
	updated, err := h.svc.Advance(ctx, workspaceID, pipelineID, currentStage, next)
	if !errors.Is(err, ErrConsecutiveAdvance) {
		return updated, err
	}
	// The prior delivery may have committed its state and lost only the next
	// message. Republish the persisted follower rather than acknowledging a
	// missing event or re-running stage work.
	current, lookupErr := h.svc.Get(ctx, workspaceID, pipelineID)
	if lookupErr != nil {
		return nil, err
	}
	if current.Status != StatusFailed && IsAfter(current.Stage, currentStage) {
		if publishErr := h.svc.RepublishCurrentEvent(ctx, current); publishErr != nil {
			return current, publishErr
		}
		return current, nil
	}
	return updated, err
}

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

func EventKind(typeName string) (Stage, bool) {
	switch strings.TrimSpace(typeName) {
	case "pipeline.research":
		return StageResearch, true
	case "pipeline.script":
		return StageScript, true
	case "pipeline.review", "pipeline.review_ready", "pipeline.review_approved":
		return StageReview, true
	case "pipeline.publish":
		return StagePublish, true
	case "pipeline.complete":
		return StageComplete, true
	default:
		return "", false
	}
}
