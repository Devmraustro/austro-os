package orchestration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// memStore is an in-memory, workspace-isolated PipelineStore for deterministic
// domain tests. It mirrors RLS isolation: a pipeline is only reachable via its
// owning workspace.
type memStore struct {
	mu    sync.Mutex
	items map[uuid.UUID]*Pipeline
}

func newMemStore() *memStore {
	return &memStore{items: map[uuid.UUID]*Pipeline{}}
}

func (m *memStore) Create(_ context.Context, p *Pipeline) (*Pipeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.items[p.ID] = &cp
	return &cp, nil
}

func (m *memStore) Get(_ context.Context, workspaceID, id uuid.UUID) (*Pipeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.items[id]
	if !ok {
		return nil, ErrNotFound
	}
	if p.WorkspaceID != workspaceID {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *memStore) List(_ context.Context, workspaceID uuid.UUID) ([]*Pipeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*Pipeline{}
	for _, p := range m.items {
		if p.WorkspaceID != workspaceID {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memStore) Update(_ context.Context, workspaceID uuid.UUID, p *Pipeline) (*Pipeline, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.items[p.ID]
	if !ok {
		return nil, ErrNotFound
	}
	if existing.WorkspaceID != workspaceID {
		return nil, ErrNotFound
	}
	cp := *p
	m.items[p.ID] = &cp
	return &cp, nil
}

func validWS() uuid.UUID { return uuid.New() }

func basePipeline(t *testing.T, store *memStore, ws uuid.UUID) *Pipeline {
	t.Helper()
	p, err := New(ws, nil, "trace-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stored, err := store.Create(context.Background(), p)
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	return stored
}

func TestFullPipelineLifecycle(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubResearcher{}, StubScriptWriter{}, StubReviewer{}, StubPublisher{}, nil, nil)
	ws := validWS()
	p := basePipeline(t, store, ws)

	if p.Stage != StageResearch || p.Status != StatusCreated {
		t.Fatalf("expected research/created, got %s/%s", p.Stage, p.Status)
	}

	// research -> script
	p, err := svc.Advance(ctx(), ws, p.ID, StageResearch, StageScript)
	if err != nil {
		t.Fatalf("research->script: %v", err)
	}
	if p.Stage != StageScript || p.Status != StatusActive {
		t.Fatalf("expected script/active, got %s/%s", p.Stage, p.Status)
	}

	// script -> review (awaiting approval)
	p, err = svc.Advance(ctx(), ws, p.ID, StageScript, StageReview)
	if err != nil {
		t.Fatalf("script->review: %v", err)
	}
	if p.Stage != StageReview || p.Status != StatusAwaitingApproval {
		t.Fatalf("expected review/awaiting_approval, got %s/%s", p.Stage, p.Status)
	}

	// Review cannot advance until a named human approval command records the
	// handoff. The publisher is still the deterministic domain stub in this
	// infrastructure-free test.
	if _, err = svc.Advance(ctx(), ws, p.ID, StageReview, StagePublish); err != ErrApprovalRequired {
		t.Fatalf("expected approval gate, got %v", err)
	}
	p, err = svc.Approve(ctx(), ws, p.ID, "operator")
	if err != nil {
		t.Fatalf("approve review: %v", err)
	}
	p, err = svc.Advance(ctx(), ws, p.ID, StageReview, StagePublish)
	if err != nil {
		t.Fatalf("review->publish: %v", err)
	}
	if p.Stage != StagePublish || p.Status != StatusActive {
		t.Fatalf("expected publish/active, got %s/%s", p.Stage, p.Status)
	}

	// publish -> complete (done)
	p, err = svc.Advance(ctx(), ws, p.ID, StagePublish, StageComplete)
	if err != nil {
		t.Fatalf("publish->complete: %v", err)
	}
	if p.Stage != StageComplete || p.Status != StatusDone {
		t.Fatalf("expected complete/done, got %s/%s", p.Stage, p.Status)
	}

	// terminal: no further advances
	if _, err := svc.Advance(ctx(), ws, p.ID, StageComplete, StageResearch); err != ErrTerminalState {
		t.Fatalf("expected ErrTerminalState, got %v", err)
	}
}

func TestInvalidAndSkippedTransitions(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubResearcher{}, StubScriptWriter{}, StubReviewer{}, StubPublisher{}, nil, nil)
	ws := validWS()
	p := basePipeline(t, store, ws)

	// skipping stages is rejected
	if _, err := svc.Advance(ctx(), ws, p.ID, StageResearch, StageReview); err != ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition skipping review, got %v", err)
	}
	// wrong current stage (stale actor) is rejected
	if _, err := svc.Advance(ctx(), ws, p.ID, StageScript, StageReview); err != ErrConsecutiveAdvance {
		t.Fatalf("expected ErrConsecutiveAdvance for stale stage, got %v", err)
	}
	// bogus stage
	if _, err := svc.Advance(ctx(), ws, p.ID, StageResearch, Stage("bogus")); err != ErrInvalidStage {
		t.Fatalf("expected ErrInvalidStage, got %v", err)
	}
}

type failOnceReviewer struct{ failed bool }

func (r *failOnceReviewer) Review(context.Context, uuid.UUID, *Pipeline) (bool, error) {
	if !r.failed {
		r.failed = true
		return false, errors.New("temporary review dependency failure")
	}
	return true, nil
}

func TestFailedStageCanOnlyRecoverThroughRetry(t *testing.T) {
	store := newMemStore()
	reviewer := &failOnceReviewer{}
	svc := NewService(store, StubResearcher{}, StubScriptWriter{}, reviewer, StubPublisher{}, nil, nil)
	ws := validWS()
	p := basePipeline(t, store, ws)

	p, err := svc.Advance(ctx(), ws, p.ID, StageResearch, StageScript)
	if err != nil {
		t.Fatalf("research->script: %v", err)
	}
	if _, err = svc.Advance(ctx(), ws, p.ID, StageScript, StageReview); err == nil {
		t.Fatal("temporary stage failure must be returned")
	}
	failed, err := svc.Get(ctx(), ws, p.ID)
	if err != nil {
		t.Fatalf("get failed pipeline: %v", err)
	}
	if failed.Status != StatusFailed || failed.FailureReason == "" {
		t.Fatalf("expected durable failure evidence, got %s/%q", failed.Status, failed.FailureReason)
	}
	if _, err = svc.Advance(ctx(), ws, p.ID, StageScript, StageReview); err != ErrTerminalState {
		t.Fatalf("failed pipeline must not be advanced directly, got %v", err)
	}

	recovered, err := svc.Retry(ctx(), ws, p.ID, "operator")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if recovered.Stage != StageReview || recovered.Status != StatusAwaitingApproval || recovered.RetryCount != 1 {
		t.Fatalf("unexpected recovery state: %s/%s retries=%d", recovered.Stage, recovered.Status, recovered.RetryCount)
	}
	if _, err = svc.Approve(ctx(), ws, p.ID, "operator"); err != nil {
		t.Fatalf("approve recovery: %v", err)
	}
	if _, err = svc.Advance(ctx(), ws, p.ID, StageReview, StagePublish); err != nil {
		t.Fatalf("publish recovery: %v", err)
	}
	if _, err = svc.Advance(ctx(), ws, p.ID, StagePublish, StageComplete); err != nil {
		t.Fatalf("complete recovery: %v", err)
	}
}

func TestWorkspaceIsolation(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubResearcher{}, StubScriptWriter{}, StubReviewer{}, StubPublisher{}, nil, nil)
	wsA, wsB := validWS(), validWS()
	p := basePipeline(t, store, wsA)

	if _, err := svc.Get(ctx(), wsB, p.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound cross-workspace get, got %v", err)
	}
	if _, err := svc.Advance(ctx(), wsB, p.ID, StageResearch, StageScript); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound cross-workspace advance, got %v", err)
	}
	all, err := svc.List(ctx(), wsB)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 for wsB, got %d", len(all))
	}
}

func TestRateLimiting(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubResearcher{}, StubScriptWriter{}, StubReviewer{}, StubPublisher{}, nil, nil)
	ws := validWS()
	p := basePipeline(t, store, ws)
	// Exhaust the per-workspace advance budget (default 10 per minute), then a
	// further advance must be refused before any state work.
	for i := 0; i < 10; i++ {
		if !svc.throttle.Allow(ws.String()) {
			t.Fatalf("throttle budget expired before 10 hits (i=%d)", i)
		}
	}
	if _, err := svc.Advance(ctx(), ws, p.ID, StageResearch, StageScript); err != ErrRateLimited {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestHandlerAndEventKind(t *testing.T) {
	h := NewHandler(NewService(newMemStore(), nil, nil, nil, nil, nil, nil))

	if _, err := NextStage(StageResearch); err != nil {
		t.Fatalf("NextStage research: %v", err)
	}
	if _, ok := EventKind("pipeline.research"); !ok {
		t.Fatal("expected pipeline.research recognized")
	}
	if _, ok := EventKind("nope"); ok {
		t.Fatal("expected unknown event unrecognized")
	}
	if _, err := NextStage(Stage("bogus")); err != ErrInvalidStage {
		t.Fatalf("expected ErrInvalidStage, got %v", err)
	}
	_ = h
}

func TestInvalidInput(t *testing.T) {
	if _, err := New(uuid.Nil, nil, "t"); err != ErrWorkspaceMismatch {
		t.Fatalf("expected ErrWorkspaceMismatch, got %v", err)
	}
	stored, err := New(validWS(), nil, "t")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if stored.TraceID != "t" {
		t.Fatalf("trace not preserved: %q", stored.TraceID)
	}
}

func ctx() context.Context {
	return context.Background()
}
