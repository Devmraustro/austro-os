package composition

import (
	"context"

	"austro-os/internal/ai"
	"austro-os/internal/config"
	"austro-os/internal/orchestration"

	"github.com/google/uuid"
)

// aiReviewer prepares the creator review step with an AI verdict. It is wired
// into the pipeline when a real AI backend is selected; with the stub backend
// the deterministic StubReviewer remains in place (ADR-010). It never bypasses
// the Step 6 human approval gate: the publish stage still requires an approved
// publication, and this reviewer only prepares the review.
type aiReviewer struct {
	ai *ai.Gateway
}

// NewAIBackedReviewer returns the AI-backed Reviewer bound to the runtime
// gateway. It is exported so tests and future wiring can reuse it; the
// runtime composition selects it via reviewerFor.
func NewAIBackedReviewer(gw *ai.Gateway) orchestration.Reviewer {
	return &aiReviewer{ai: gw}
}

// Review classifies the pipeline artefact against the creator principles. A
// compliant verdict passes the review preparation; any other verdict fails the
// stage so the human gate still governs the final publish decision. The
// gateway enforces the workspace usage budget, so an exhausted budget surfaces
// as an error and fails the advance (fail-closed).
func (r *aiReviewer) Review(ctx context.Context, workspaceID uuid.UUID, p *orchestration.Pipeline) (bool, error) {
	res, err := r.ai.Classify(ctx, ai.ClassificationRequest{
		Scope:      ai.Scope{WorkspaceID: workspaceID.String()},
		Categories: []string{"compliant", "needs_review"},
		Input:      string(p.Stage) + ":" + p.ID.String(),
	})
	if err != nil {
		return false, err
	}
	return res.Category == "compliant", nil
}

// reviewerFor chooses the review capability for the configured AI backend.
// The default (stub) backend keeps the deterministic reviewer; any real backend
// routes the review preparation through the AI gateway.
func reviewerFor(gw *ai.Gateway, aiBackend string) orchestration.Reviewer {
	if aiBackend == "" || aiBackend == config.AIBackendStub {
		return orchestration.StubReviewer{}
	}
	return NewAIBackedReviewer(gw)
}
