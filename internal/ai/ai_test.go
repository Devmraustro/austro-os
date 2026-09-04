package ai_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"austro-os/internal/ai"
)

func scoped(ws string) ai.Scope {
	return ai.Scope{WorkspaceID: ws}
}

// failingProvider is a replaceable Provider substitute that always errors,
// used to prove callers depend only on the port.
type failingProvider struct{}

func (failingProvider) Complete(context.Context, ai.CompletionRequest) (ai.CompletionResult, error) {
	return ai.CompletionResult{}, errors.New("boom")
}
func (failingProvider) Embed(context.Context, ai.EmbedRequest) (ai.EmbeddingResult, error) {
	return ai.EmbeddingResult{}, errors.New("boom")
}
func (failingProvider) Classify(context.Context, ai.ClassificationRequest) (ai.ClassificationResult, error) {
	return ai.ClassificationResult{}, errors.New("boom")
}

type recordingSink struct {
	records []ai.DecisionRecord
}

func (r *recordingSink) Sink(d ai.DecisionRecord) {
	r.records = append(r.records, d)
}

func newGateway(t *testing.T, p ai.Provider, g ai.UsageGuard) *ai.Gateway {
	t.Helper()
	gw, err := ai.NewGateway(ai.ConfigAPI{Provider: p, Guard: g, Sink: &recordingSink{}})
	require.NoError(t, err)
	return gw
}

func TestStubCompleteIsDeterministic(t *testing.T) {
	var p ai.StubProvider
	req := ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "write a script"}
	r1, err := p.Complete(context.Background(), req)
	require.NoError(t, err)
	r2, err := p.Complete(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, r1, r2, "stub output must be a pure function of its input")
	assert.NotEmpty(t, r1.Text)
}

func TestStubCompleteEmptyInstruction(t *testing.T) {
	var p ai.StubProvider
	_, err := p.Complete(context.Background(), ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "  "})
	require.ErrorIs(t, err, ai.ErrInvalidInput)
	require.ErrorContains(t, err, "instruction")
}

func TestStubCompleteMissingWorkspace(t *testing.T) {
	var p ai.StubProvider
	_, err := p.Complete(context.Background(), ai.CompletionRequest{Scope: scoped(""), Instruction: "x"})
	require.ErrorIs(t, err, ai.ErrWorkspaceRequired)
}

func TestStubEmbedDimensionConsistency(t *testing.T) {
	var p ai.StubProvider
	r1, err := p.Embed(context.Background(), ai.EmbedRequest{Scope: scoped("ws-a"), Content: "hello", Dimensions: 4})
	require.NoError(t, err)
	require.Len(t, r1.Values, 4)
	require.Equal(t, 4, r1.Dimensions)
	r2, err := p.Embed(context.Background(), ai.EmbedRequest{Scope: scoped("ws-a"), Content: "hello", Dimensions: 4})
	require.NoError(t, err)
	require.Equal(t, r1.Values, r2.Values, "embed must be deterministic for identical input")
}

func TestStubEmbedInvalidDimensions(t *testing.T) {
	var p ai.StubProvider
	_, err := p.Embed(context.Background(), ai.EmbedRequest{Scope: scoped("ws-a"), Content: "x", Dimensions: 0})
	require.ErrorIs(t, err, ai.ErrInvalidInput)
	_, err = p.Embed(context.Background(), ai.EmbedRequest{Scope: scoped("ws-a"), Content: "x", Dimensions: -1})
	require.ErrorIs(t, err, ai.ErrInvalidInput)
}

func TestStubEmbedEmptyContent(t *testing.T) {
	var p ai.StubProvider
	_, err := p.Embed(context.Background(), ai.EmbedRequest{Scope: scoped("ws-a"), Content: " ", Dimensions: 3})
	require.ErrorIs(t, err, ai.ErrInvalidInput)
	require.ErrorContains(t, err, "content")
}

func TestStubClassifyDeterministicAndInCategories(t *testing.T) {
	var p ai.StubProvider
	cats := []string{"educational", "marketing", "islamic"}
	res, err := p.Classify(context.Background(), ai.ClassificationRequest{Scope: scoped("ws-a"), Categories: cats, Input: "history of islam"})
	require.NoError(t, err)
	assert.Contains(t, cats, res.Category)
	res2, err := p.Classify(context.Background(), ai.ClassificationRequest{Scope: scoped("ws-a"), Categories: cats, Input: "history of islam"})
	require.NoError(t, err)
	assert.Equal(t, res, res2)
}

func TestStubClassifyInvalid(t *testing.T) {
	var p ai.StubProvider
	_, err := p.Classify(context.Background(), ai.ClassificationRequest{Scope: scoped("ws-a"), Categories: nil, Input: "x"})
	require.ErrorIs(t, err, ai.ErrInvalidInput)
	_, err = p.Classify(context.Background(), ai.ClassificationRequest{Scope: scoped("ws-a"), Categories: []string{"a"}, Input: " "})
	require.ErrorIs(t, err, ai.ErrInvalidInput)
}

func TestGatewayRequiresProviderAndGuard(t *testing.T) {
	_, err := ai.NewGateway(ai.ConfigAPI{Provider: nil, Guard: ai.NoopGuard{}, Sink: nil})
	require.Error(t, err)
	_, err = ai.NewGateway(ai.ConfigAPI{Provider: ai.StubProvider{}, Guard: nil, Sink: nil})
	require.Error(t, err)
}

func TestGatewayEnforcesWorkspaceAtBoundary(t *testing.T) {
	gw := newGateway(t, ai.StubProvider{}, ai.NoopGuard{})
	_, err := gw.Complete(context.Background(), ai.CompletionRequest{Scope: scoped(""), Instruction: "x"})
	require.ErrorIs(t, err, ai.ErrWorkspaceRequired)
	_, err = gw.Embed(context.Background(), ai.EmbedRequest{Scope: scoped(""), Content: "x", Dimensions: 2})
	require.ErrorIs(t, err, ai.ErrWorkspaceRequired)
	_, err = gw.Classify(context.Background(), ai.ClassificationRequest{Scope: scoped(""), Categories: []string{"a"}, Input: "x"})
	require.ErrorIs(t, err, ai.ErrWorkspaceRequired)
}

func TestGatewayReplaceabilityWithFailingProvider(t *testing.T) {
	// A different Provider honouring the same port is substituted without
	// changing the caller; here it surfaces a provider failure cleanly.
	gw := newGateway(t, failingProvider{}, ai.NoopGuard{})
	_, err := gw.Complete(context.Background(), ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "x"})
	require.Error(t, err)
	require.ErrorContains(t, err, "boom")
}

func TestGatewayAuditEmissionSuccess(t *testing.T) {
	sink := &recordingSink{}
	gw, err := ai.NewGateway(ai.ConfigAPI{Provider: ai.StubProvider{}, Guard: ai.NoopGuard{}, Sink: sink})
	require.NoError(t, err)
	_, err = gw.Complete(context.Background(), ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "x"})
	require.NoError(t, err)
	require.Len(t, sink.records, 1)
	rec := sink.records[0]
	assert.Equal(t, "ai.complete", rec.EventType)
	assert.Equal(t, "success", rec.Outcome)
	assert.Equal(t, "ws-a", rec.WorkspaceID)
	assert.NotEmpty(t, rec.ConstitutionalPrinciple)
}

func TestGatewayAuditEmissionFailure(t *testing.T) {
	sink := &recordingSink{}
	gw, err := ai.NewGateway(ai.ConfigAPI{Provider: failingProvider{}, Guard: ai.NoopGuard{}, Sink: sink})
	require.NoError(t, err)
	_, err = gw.Complete(context.Background(), ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "x"})
	require.Error(t, err)
	require.Len(t, sink.records, 1)
	assert.Equal(t, "failed", sink.records[0].Outcome)
}

func TestConfigurableUsageGuardBlocksAfterBudget(t *testing.T) {
	g := ai.NewConfigurableUsageGuard(ai.Config{MaxPerWorkspace: 2})
	gw := newGateway(t, ai.StubProvider{}, g)
	ctx := context.Background()
	req := ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "x"}
	res, err := gw.Complete(ctx, req)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Text)
	_, err = gw.Complete(ctx, req)
	require.NoError(t, err)
	_, err = gw.Complete(ctx, req)
	require.ErrorIs(t, err, ai.ErrUsageLimitExceeded)
	assert.Equal(t, uint64(2), g.Used("ws-a"))
}

func TestUsageGuardRecordsUsageWithoutCeiling(t *testing.T) {
	g := ai.NewConfigurableUsageGuard(ai.Config{}) // no ceiling
	gw := newGateway(t, ai.StubProvider{}, g)
	ctx := context.Background()
	req := ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "x"}
	for i := 0; i < 5; i++ {
		_, err := gw.Complete(ctx, req)
		require.NoError(t, err)
	}
	// Unlimited guardianship must still record usage so operators can observe it.
	require.Equal(t, uint64(5), g.Used("ws-a"))
	require.Equal(t, uint64(0), g.Used("ws-untouched"), "unused workspace must record nothing")
}

func TestUsageGuardIsWorkspaceIsolated(t *testing.T) {
	g := ai.NewConfigurableUsageGuard(ai.Config{MaxPerWorkspace: 1})
	gw := newGateway(t, ai.StubProvider{}, g)
	ctx := context.Background()
	// ws-a exhausts its budget.
	_, err := gw.Complete(ctx, ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "x"})
	require.NoError(t, err)
	_, err = gw.Complete(ctx, ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "x"})
	require.ErrorIs(t, err, ai.ErrUsageLimitExceeded)
	// ws-b is unaffected (no cross-workspace leakage into the guard).
	_, err = gw.Complete(ctx, ai.CompletionRequest{Scope: scoped("ws-b"), Instruction: "y"})
	require.NoError(t, err)
}

func TestCrossWorkspaceCallsDoNotShareStubState(t *testing.T) {
	// The stub is stateless: identical inputs in different workspaces yield
	// identical outputs, and calls in ws-b never depend on a prior ws-a call.
	var p ai.StubProvider
	rA, err := p.Complete(context.Background(), ai.CompletionRequest{Scope: scoped("ws-a"), Instruction: "same"})
	require.NoError(t, err)
	rB, err := p.Complete(context.Background(), ai.CompletionRequest{Scope: scoped("ws-b"), Instruction: "same"})
	require.NoError(t, err)
	require.Equal(t, rA, rB, "stub has no cross-workspace state; outputs depend only on inputs")
}

func TestNullSinkIsNoop(t *testing.T) {
	ai.NullSink{}.Sink(ai.DecisionRecord{}) // must not panic or record
}

func TestTraceSpanPropagatedToAuditRecord(t *testing.T) {
	sink := &recordingSink{}
	gw, err := ai.NewGateway(ai.ConfigAPI{Provider: ai.StubProvider{}, Guard: ai.NoopGuard{}, Sink: sink})
	require.NoError(t, err)
	tid := uuid.New()
	sid := uuid.New()
	scope := ai.Scope{WorkspaceID: "ws-a", TraceID: tid, SpanID: sid}
	_, err = gw.Embed(context.Background(), ai.EmbedRequest{Scope: scope, Content: "x", Dimensions: 2})
	require.NoError(t, err)
	require.Len(t, sink.records, 1)
	assert.Equal(t, tid, sink.records[0].TraceID)
	assert.Equal(t, sid, sink.records[0].SpanID)
}
