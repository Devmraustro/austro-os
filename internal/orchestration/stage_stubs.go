package orchestration

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Stage stubs — deterministic, side-effect free capability ports (ADR-010,
// ROADMAP §5.8/§5.9). They perform no real campaign research, no AI script
// generation, no live review, and no real platform publish. Each return is a
// pure function of the pipeline so the orchestration loop is replay-safe.

// StubResearcher is the deterministic research stub.
type StubResearcher struct{}

func (StubResearcher) Research(_ context.Context, workspaceID uuid.UUID, p *Pipeline) (string, error) {
	return fmt.Sprintf("research://%s/%s", workspaceID, p.ID), nil
}

// StubScriptWriter produces a deterministic script reference.
type StubScriptWriter struct{}

func (StubScriptWriter) WriteScript(_ context.Context, workspaceID uuid.UUID, p *Pipeline, researchRef string) (string, error) {
	return fmt.Sprintf("script://%s/%s/%s", workspaceID, p.ID, researchRef), nil
}

// StubReviewer returns a passing review deterministically.
type StubReviewer struct{}

func (StubReviewer) Review(_ context.Context, _ uuid.UUID, _ *Pipeline) (bool, error) {
	return true, nil
}

// StubPublisher is the deterministic, approval-gated publisher used in Phase 2.
// It never contacts a real platform and performs no live external write.
type StubPublisher struct{}

func (StubPublisher) Publish(_ context.Context, workspaceID uuid.UUID, p *Pipeline, reviewRef string) (string, error) {
	return fmt.Sprintf("stub-publish://%s/%s/%s", workspaceID, p.ID, reviewRef), nil
}
