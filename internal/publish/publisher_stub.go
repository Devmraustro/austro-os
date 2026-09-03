package publish

import (
	"context"
	"fmt"
)

// StubPublisher is the deterministic, side-effect free platform adapter for
// Phase 2 (ADR-009, ROADMAP §5.10). It never contacts a real platform, holds no
// credential, and performs no live external write. Its output — a stable
// external reference derived only from the publication's content digest and id —
// is a pure function of the request, so re-publishing is replay-safe and
// idempotent.
type StubPublisher struct{}

// Publish returns a deterministic external reference for an approved
// publication. It refuses any publication that is not approved (a second,
// in-adapter guard for the Human Oversight gate).
func (StubPublisher) Publish(_ context.Context, p *Publication) (string, error) {
	if p == nil || !p.HasApproval() {
		return "", ErrApprovalRequired
	}
	return fmt.Sprintf("stub://%s/%s", p.Platform, p.ContentHash), nil
}
