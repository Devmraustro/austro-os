package composition_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"austro-os/internal/ai"
	"austro-os/internal/composition"
	"austro-os/internal/config"
	"austro-os/internal/orchestration"
	"austro-os/internal/publish"

	"github.com/google/uuid"
)

// ---- in-memory persistence ports (mirror the domain test fakes) ----

type memPublications struct {
	mu    sync.Mutex
	items map[uuid.UUID]*publish.Publication
}

func newMemPublications() *memPublications {
	return &memPublications{items: map[uuid.UUID]*publish.Publication{}}
}

func (m *memPublications) Create(_ context.Context, p *publish.Publication) (*publish.Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.items[p.ID] = &cp
	return &cp, nil
}

func (m *memPublications) Get(_ context.Context, workspaceID, id uuid.UUID) (*publish.Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.items[id]
	if !ok || p.WorkspaceID != workspaceID {
		return nil, publish.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *memPublications) List(_ context.Context, workspaceID uuid.UUID, status *publish.Status) ([]*publish.Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*publish.Publication{}
	for _, p := range m.items {
		if p.WorkspaceID != workspaceID {
			continue
		}
		if status != nil && p.Status != *status {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memPublications) Update(_ context.Context, workspaceID uuid.UUID, p *publish.Publication) (*publish.Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.items[p.ID]
	if !ok {
		return nil, publish.ErrNotFound
	}
	if existing.WorkspaceID != workspaceID {
		return nil, publish.ErrWorkspaceMismatch
	}
	cp := *p
	m.items[p.ID] = &cp
	return &cp, nil
}

var _ publish.PublicationStore = (*memPublications)(nil)

type memPipelines struct {
	mu    sync.Mutex
	items map[uuid.UUID]*orchestration.Pipeline
}

func newMemPipelines() *memPipelines {
	return &memPipelines{items: map[uuid.UUID]*orchestration.Pipeline{}}
}

func (m *memPipelines) Create(_ context.Context, p *orchestration.Pipeline) (*orchestration.Pipeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.items[p.ID] = &cp
	return &cp, nil
}

func (m *memPipelines) Get(_ context.Context, workspaceID, id uuid.UUID) (*orchestration.Pipeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.items[id]
	if !ok || p.WorkspaceID != workspaceID {
		return nil, orchestration.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *memPipelines) List(_ context.Context, workspaceID uuid.UUID) ([]*orchestration.Pipeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*orchestration.Pipeline{}
	for _, p := range m.items {
		if p.WorkspaceID != workspaceID {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memPipelines) Update(_ context.Context, workspaceID uuid.UUID, p *orchestration.Pipeline) (*orchestration.Pipeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.items[p.ID]
	if !ok {
		return nil, orchestration.ErrNotFound
	}
	if existing.WorkspaceID != workspaceID {
		return nil, orchestration.ErrWorkspaceMismatch
	}
	cp := *p
	m.items[p.ID] = &cp
	return &cp, nil
}

var _ orchestration.PipelineStore = (*memPipelines)(nil)

// ---- helpers ----

func stubCfg() *config.Config {
	return &config.Config{
		AIBackend:      config.AIBackendStub,
		PublishBackend: config.PublishBackendStub,
		Environment:    "test",
		RabbitMQQueue:  "austro.events",
		ServerAddress:  ":0",
	}
}

func composeStub() (*composition.Runtime, error) {
	return composition.Compose(stubCfg(), composition.Stores{
		Publications: newMemPublications(),
		Pipelines:    newMemPipelines(),
	}, composition.Sinks{})
}

// openAIStub spins up an OpenAI-compatible endpoint returning the given label.
func openAIStub(t *testing.T, label string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + label + `"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- tests ----

func TestComposeDefaultsToStubs(t *testing.T) {
	rt, err := composeStub()
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if rt.AIBackend != config.AIBackendStub || rt.PublishBackend != config.PublishBackendStub {
		t.Fatalf("expected stub backends, got %q/%q", rt.AIBackend, rt.PublishBackend)
	}
	if _, ok := rt.AIProvider.(ai.StubProvider); !ok {
		t.Fatalf("expected ai.StubProvider, got %T", rt.AIProvider)
	}
	if _, ok := rt.Publisher.(publish.StubPublisher); !ok {
		t.Fatalf("expected publish.StubPublisher, got %T", rt.Publisher)
	}
	if rt.AI == nil || rt.Publish == nil || rt.Orchestration == nil || rt.Handler == nil {
		t.Fatalf("runtime services must be wired")
	}
}

func TestComposeSelectsRealAdapters(t *testing.T) {
	cfg := stubCfg()
	cfg.AIBackend = config.AIBackendOpenAICompatible
	cfg.AIModel = "austro-classifier"
	cfg.AIBaseURL = "https://ai.austro.internal/v1"
	cfg.AIAPIKey = "sk-test-1a2b3c4d5e6f"
	cfg.PublishBackend = config.PublishBackendGenericHTTP
	cfg.PublishWebhookURL = "https://hook.austro.internal/deliver"
	cfg.PublishToken = "tok-test-1a2b3c4d5e6f"

	rt, err := composition.Compose(cfg, composition.Stores{
		Publications: newMemPublications(),
		Pipelines:    newMemPipelines(),
	}, composition.Sinks{})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if rt.AIBackend != config.AIBackendOpenAICompatible {
		t.Fatalf("expected openai-compatible ai backend, got %q", rt.AIBackend)
	}
	if rt.PublishBackend != config.PublishBackendGenericHTTP {
		t.Fatalf("expected generic-http publish backend, got %q", rt.PublishBackend)
	}
	if _, ok := rt.AIProvider.(*ai.OpenAICompatibleProvider); !ok {
		t.Fatalf("expected *ai.OpenAICompatibleProvider, got %T", rt.AIProvider)
	}
	if _, ok := rt.Publisher.(*publish.GenericHTTPPublisher); !ok {
		t.Fatalf("expected *publish.GenericHTTPPublisher, got %T", rt.Publisher)
	}
}

func TestComposeFailsClosedOnUnknownBackends(t *testing.T) {
	cfg := stubCfg()
	cfg.AIBackend = "mystery-model"
	if _, err := composition.Compose(cfg, composition.Stores{
		Publications: newMemPublications(),
		Pipelines:    newMemPipelines(),
	}, composition.Sinks{}); err == nil {
		t.Fatalf("expected error for unknown ai backend")
	} else if !errors.Is(err, ai.ErrProviderConfig) {
		t.Fatalf("expected ai.ErrProviderConfig, got %v", err)
	}

	cfg.AIBackend = config.AIBackendStub
	cfg.PublishBackend = "mystery-http"
	if _, err := composition.Compose(cfg, composition.Stores{
		Publications: newMemPublications(),
		Pipelines:    newMemPipelines(),
	}, composition.Sinks{}); err == nil {
		t.Fatalf("expected error for unknown publish backend")
	} else if !errors.Is(err, publish.ErrPublisherConfig) {
		t.Fatalf("expected publish.ErrPublisherConfig, got %v", err)
	}
}

func TestComposeWiresUsageGuardFromConfig(t *testing.T) {
	cfg := stubCfg()
	cfg.AIUsageLimitPerWorkspace = 7
	rt, err := composition.Compose(cfg, composition.Stores{
		Publications: newMemPublications(),
		Pipelines:    newMemPipelines(),
	}, composition.Sinks{})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	guard, ok := rt.UsageGuard.(*ai.ConfigurableUsageGuard)
	if !ok {
		t.Fatalf("expected *ai.ConfigurableUsageGuard, got %T", rt.UsageGuard)
	}
	// The budget must be derived from cfg: the 7th allowance succeeds and the
	// 8th is rejected.
	for i := uint64(0); i < cfg.AIUsageLimitPerWorkspace; i++ {
		allowed, err := guard.Allow("ws-a", 1)
		if err != nil {
			t.Fatalf("Allow #%d: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("Allow #%d rejected within budget", i+1)
		}
	}
	if allowed, err := guard.Allow("ws-a", 1); err != nil || allowed {
		t.Fatalf("expected budget exceeded (allowed=%v, err=%v)", allowed, err)
	}
}

// TestAIReviewerEnforcementThroughGateway proves the composed runtime routes the
// pipeline review step through the real gateway: one compliant verdict consumes
// the budget, and a second review fails closed with the usage-limit error.
func TestAIReviewerEnforcementThroughGateway(t *testing.T) {
	srv := openAIStub(t, "compliant")

	cfg := stubCfg()
	cfg.AIBackend = config.AIBackendOpenAICompatible
	cfg.AIModel = "austro-classifier"
	cfg.AIBaseURL = srv.URL
	cfg.AIAPIKey = "sk-test-1a2b3c4d5e6f"
	cfg.AIUsageLimitPerWorkspace = 1

	pipelines := newMemPipelines()
	rt, err := composition.Compose(cfg, composition.Stores{
		Publications: newMemPublications(),
		Pipelines:    pipelines,
	}, composition.Sinks{})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}

	ctx := context.Background()
	ws := uuid.New()
	pA, err := rt.Orchestration.Create(ctx, ws, nil, "trace-a")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := rt.Orchestration.Advance(ctx, ws, pA.ID, orchestration.StageResearch, orchestration.StageScript); err != nil {
		t.Fatalf("advance to script: %v", err)
	}
	// Review consumes the single budgeted operation and passes (compliant).
	if _, err := rt.Orchestration.Advance(ctx, ws, pA.ID, orchestration.StageScript, orchestration.StageReview); err != nil {
		t.Fatalf("advance to review: %v", err)
	}

	pB, err := rt.Orchestration.Create(ctx, ws, nil, "trace-b")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := rt.Orchestration.Advance(ctx, ws, pB.ID, orchestration.StageResearch, orchestration.StageScript); err != nil {
		t.Fatalf("advance to script: %v", err)
	}
	// Budget exhausted: the review fails closed with the usage-limit error.
	if _, err := rt.Orchestration.Advance(ctx, ws, pB.ID, orchestration.StageScript, orchestration.StageReview); !errors.Is(err, ai.ErrUsageLimitExceeded) {
		t.Fatalf("expected usage limit error, got %v", err)
	}
}

// TestComposeStubReviewerBypassesGateway proves the default composition never
// calls an external endpoint: with the stub backend the pipeline advances
// through review deterministically.
func TestComposeStubReviewerBypassesGateway(t *testing.T) {
	rt, err := composeStub()
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	ctx := context.Background()
	ws := uuid.New()
	p, err := rt.Orchestration.Create(ctx, ws, nil, "trace")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := rt.Orchestration.Advance(ctx, ws, p.ID, orchestration.StageResearch, orchestration.StageScript); err != nil {
		t.Fatalf("advance to script: %v", err)
	}
	if _, err := rt.Orchestration.Advance(ctx, ws, p.ID, orchestration.StageScript, orchestration.StageReview); err != nil {
		t.Fatalf("advance to review: %v", err)
	}
	if _, ok := rt.AIProvider.(ai.StubProvider); !ok {
		t.Fatalf("expected stub provider under stub backend, got %T", rt.AIProvider)
	}
}
