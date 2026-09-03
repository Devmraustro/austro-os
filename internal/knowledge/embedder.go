package knowledge

import (
	"context"

	"austro-os/internal/ai"

	"github.com/google/uuid"
)

// GatewayEmbedder adapts the provider-agnostic AI gateway into the knowledge
// Embedder port. It enforces a non-empty workspace on each call (deny-by-
// default) and delegates embedding to the replaceable Provider. Because it
// depends only on the Provider port, it preserves AI Independence / P8 without
// coupling the knowledge domain to any concrete backend.
type GatewayEmbedder struct {
	provider ai.Provider
}

// NewGatewayEmbedder returns an Embedder backed by an ai.Provider.
func NewGatewayEmbedder(provider ai.Provider) *GatewayEmbedder {
	return &GatewayEmbedder{provider: provider}
}

// Embed produces a fixed-size embedding for content within the given workspace.
// It rejects empty workspaces and lets the provider validate content size.
func (g *GatewayEmbedder) Embed(ctx context.Context, workspaceID uuid.UUID, content string, dimensions int) ([]float32, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	res, err := g.provider.Embed(ctx, ai.EmbedRequest{
		Scope:      ai.Scope{WorkspaceID: workspaceID.String()},
		Content:    content,
		Dimensions: dimensions,
	})
	if err != nil {
		return nil, err
	}
	return res.Values, nil
}
