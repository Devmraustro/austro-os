package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"austro-os/internal/auth"
	"austro-os/internal/orchestration"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type pipelineCreateProbe struct {
	createCalls int
}

func (p *pipelineCreateProbe) CreateWithIdempotency(_ context.Context, workspaceID uuid.UUID, goalID *uuid.UUID, traceID, key string) (*orchestration.Pipeline, error) {
	p.createCalls++
	return orchestration.NewWithIdempotency(workspaceID, goalID, traceID, key)
}

func (p *pipelineCreateProbe) Get(context.Context, uuid.UUID, uuid.UUID) (*orchestration.Pipeline, error) {
	return nil, orchestration.ErrNotFound
}

func (p *pipelineCreateProbe) List(context.Context, uuid.UUID) ([]*orchestration.Pipeline, error) {
	return nil, nil
}

func (p *pipelineCreateProbe) Approve(context.Context, uuid.UUID, uuid.UUID, string) (*orchestration.Pipeline, error) {
	return nil, orchestration.ErrNotFound
}

func (p *pipelineCreateProbe) Retry(context.Context, uuid.UUID, uuid.UUID, string) (*orchestration.Pipeline, error) {
	return nil, orchestration.ErrNotFound
}

// TestPipelineCreateRejectsClientControlledStageStatus is the regression guard
// for the server-authoritative lifecycle. Stage and status are response fields,
// never accepted create inputs; accepting either would let a client skip the
// worker and approval gates before the aggregate exists.
func TestPipelineCreateRejectsClientControlledStageStatus(t *testing.T) {
	workspaceID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	claims := &auth.Claims{
		ID:          "aaaaaaaa-0000-4000-8000-000000000001",
		Role:        "workspace_member",
		WorkspaceID: workspaceID.String(),
	}

	for _, field := range []string{"stage", "status"} {
		t.Run(field, func(t *testing.T) {
			probe := &pipelineCreateProbe{}
			h := NewPipelineHandler(probe)
			payload := `{"` + field + `":"publish"}`
			req := httptest.NewRequest(http.MethodPost, "/pipelines", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			req = auth.WithClaims(req, claims)
			rec := httptest.NewRecorder()

			h.Create(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, 0, probe.createCalls)
		})
	}
}
