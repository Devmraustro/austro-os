package composition

import (
	"context"
	"fmt"

	"austro-os/internal/orchestration"
	"austro-os/internal/publish"

	"github.com/google/uuid"
)

// pipelinePublicationAdapter is the only bridge from orchestration to the
// existing Publishing service. It creates one real persisted draft per
// pipeline, reuses Publishing's approval gate and publisher, and uses the
// pipeline id as the stable creation key for recovery.
type pipelinePublicationAdapter struct{ svc *publish.Service }

func (a pipelinePublicationAdapter) Prepare(ctx context.Context, workspaceID uuid.UUID, p *orchestration.Pipeline) (uuid.UUID, error) {
	if a.svc == nil || p == nil {
		return uuid.Nil, fmt.Errorf("pipeline publication service unavailable")
	}
	body := fmt.Sprintf("Research reference: %s\nScript reference: %s\nReview reference: %s", p.ResearchReference, p.ScriptReference, p.ReviewReference)
	pub, err := a.svc.CreateWithIdempotency(ctx, workspaceID, p.GoalID, p.TaskID, "Creator pipeline output", body, publish.StubPlatform, "pipeline:"+p.ID.String())
	if err != nil {
		return uuid.Nil, err
	}
	switch pub.Status {
	case publish.StatusQueued:
		pub, err = a.svc.ToReview(ctx, workspaceID, pub.ID)
		if err != nil {
			return uuid.Nil, err
		}
	case publish.StatusReview, publish.StatusApproved, publish.StatusPublished:
		// An earlier attempt may have persisted the publication before the
		// pipeline update. The idempotent record is the recovery source.
	default:
		return uuid.Nil, fmt.Errorf("pipeline publication is not reviewable: %s", pub.Status)
	}
	return pub.ID, nil
}

func (a pipelinePublicationAdapter) Approve(ctx context.Context, workspaceID, publicationID uuid.UUID, actorID string) error {
	pub, err := a.svc.Get(ctx, workspaceID, publicationID)
	if err != nil {
		return err
	}
	if pub.Status == publish.StatusApproved || pub.Status == publish.StatusPublished {
		return nil
	}
	_, err = a.svc.Approve(ctx, workspaceID, publicationID, actorID)
	return err
}

func (a pipelinePublicationAdapter) Publish(ctx context.Context, workspaceID, publicationID uuid.UUID, actorID string) (string, error) {
	pub, err := a.svc.Get(ctx, workspaceID, publicationID)
	if err != nil {
		return "", err
	}
	if pub.Status == publish.StatusPublished {
		return pub.ExternalReference, nil
	}
	if pub.Status == publish.StatusFailed {
		pub, err = a.svc.Retry(ctx, workspaceID, publicationID, actorID)
	} else {
		pub, err = a.svc.Publish(ctx, workspaceID, publicationID, actorID)
	}
	if err != nil {
		return "", err
	}
	return pub.ExternalReference, nil
}
