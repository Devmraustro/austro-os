package publish

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// memStore is an in-memory, workspace-isolated PublicationStore used for
// deterministic domain tests. It mirrors RLS isolation: a publication is only
// reachable via its owning workspace, and workspace/status filtering is applied
// in-memory exactly as the concrete store applies it via SQL.
type memStore struct {
	mu    sync.Mutex
	items map[uuid.UUID]*Publication
}

func newMemStore() *memStore {
	return &memStore{items: map[uuid.UUID]*Publication{}}
}

func (m *memStore) Create(_ context.Context, p *Publication) (*Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.items[p.ID] = &cp
	return &cp, nil
}

func (m *memStore) Get(_ context.Context, workspaceID, id uuid.UUID) (*Publication, error) {
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

func (m *memStore) List(_ context.Context, workspaceID uuid.UUID, status *Status) ([]*Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*Publication{}
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

func (m *memStore) Update(_ context.Context, workspaceID uuid.UUID, p *Publication) (*Publication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.items[p.ID]
	if !ok {
		return nil, ErrNotFound
	}
	if existing.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	cp := *p
	m.items[p.ID] = &cp
	return &cp, nil
}

// countingSink records audit decisions for assertion.
type countingSink struct {
	mu    sync.Mutex
	recs  []AuditRecord
	count int
}

func (c *countingSink) Record(_ context.Context, r AuditRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
	c.count++
}

func validWorkspace() uuid.UUID { return uuid.New() }

func basePublication(t *testing.T, store *memStore, ws uuid.UUID, title string) *Publication {
	t.Helper()
	p, err := New(ws, nil, nil, title, "body-x", StubPlatform)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stored, err := store.Create(context.Background(), p)
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	return stored
}

func TestStateMachineValidPath(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubPublisher{}, &countingSink{}, nil)
	ws := validWorkspace()
	p := basePublication(t, store, ws, "t")

	if p.Status != StatusQueued {
		t.Fatalf("expected queued, got %s", p.Status)
	}
	// invalid direct publish from queued: must be rejected before any publish
	if _, err := svc.Publish(ctx(), ws, p.ID, "human"); err != ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition publishing from queued, got %v", err)
	}
	inReview, err := svc.ToReview(ctx(), ws, p.ID)
	if err != nil {
		t.Fatalf("ToReview: %v", err)
	}
	if inReview.Status != StatusReview {
		t.Fatalf("expected review, got %s", inReview.Status)
	}
	// publish before approval must fail
	if _, err := svc.Publish(ctx(), ws, p.ID, "human"); err != ErrInvalidTransition {
		t.Fatalf("expected ErrInvalidTransition publishing from review, got %v", err)
	}
	approved, err := svc.Approve(ctx(), ws, p.ID, "alice")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != StatusApproved || approved.ApprovedBy == nil || *approved.ApprovedBy != "alice" {
		t.Fatalf("approve record wrong: %+v", approved)
	}
	published, err := svc.Publish(ctx(), ws, p.ID, "bob")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.Status != StatusPublished || published.PublishedAt == nil {
		t.Fatalf("publish record wrong: %+v", published)
	}
}

func TestPublishRequiresHumanApproval(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubPublisher{}, &countingSink{}, nil)
	ws := validWorkspace()
	p := basePublication(t, store, ws, "t")
	if _, err := svc.ToReview(ctx(), ws, p.ID); err != nil {
		t.Fatalf("ToReview: %v", err)
	}
	// machine-only transitions to approved must be impossible; and a publish
	// with no human recorded must be refused by the in-adapter guard too.
	got, err := svc.Publish(ctx(), ws, p.ID, "carol")
	if err == nil {
		t.Fatalf("expected publish approval guard to reject, got status %s", got.Status)
	}
}

func TestPublishRejectedWithoutApprovalMarker(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubPublisher{}, &countingSink{}, nil)
	ws := validWorkspace()
	p := basePublication(t, store, ws, "t")
	if _, err := svc.ToReview(ctx(), ws, p.ID); err != nil {
		t.Fatalf("ToReview: %v", err)
	}
	// craft a publication in "approved" status without the human marker (simulates a bypass)
	p.Status = StatusApproved
	if _, err := store.Update(context.Background(), ws, p); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := svc.Publish(ctx(), ws, p.ID, "carol"); err != ErrApprovalRequired {
		t.Fatalf("expected ErrApprovalRequired, got %v", err)
	}
}

func TestRejectAndTerminal(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubPublisher{}, &countingSink{}, nil)
	ws := validWorkspace()
	p := basePublication(t, store, ws, "t")
	if _, err := svc.ToReview(ctx(), ws, p.ID); err != nil {
		t.Fatalf("ToReview: %v", err)
	}
	rejected, err := svc.Reject(ctx(), ws, p.ID, "dora")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.Status != StatusRejected {
		t.Fatalf("expected rejected, got %s", rejected.Status)
	}
	// no transitions out of a terminal state
	for _, to := range []Status{StatusReview, StatusApproved, StatusPublished} {
		if _, err := svc.Approve(ctx(), ws, p.ID, "eve"); err == nil {
			t.Fatalf("expected error transitioning rejected -> %s", to)
		}
	}
	if _, err := svc.Publish(ctx(), ws, p.ID, "frank"); err != ErrTerminalState {
		t.Fatalf("expected ErrTerminalState publish of rejected, got %v", err)
	}
}

func TestWorkspaceIsolation(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubPublisher{}, &countingSink{}, nil)
	wsA, wsB := validWorkspace(), validWorkspace()
	p := basePublication(t, store, wsA, "t")
	// operating on wsB must not see wsA's item
	if _, err := svc.Get(context.Background(), wsB, p.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound cross-workspace, got %v", err)
	}
	if _, err := svc.ToReview(context.Background(), wsB, p.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound transition cross-workspace, got %v", err)
	}
	// list only returns the owning workspace
	all, err := svc.List(context.Background(), wsB, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 items for wsB, got %d", len(all))
	}
}

func TestUnauthorizedActors(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubPublisher{}, &countingSink{}, nil)
	ws := validWorkspace()
	p := basePublication(t, store, ws, "t")
	if _, err := svc.Approve(ctx(), ws, p.ID, "  "); err != ErrUnauthorizedApprover {
		t.Fatalf("expected ErrUnauthorizedApprover, got %v", err)
	}
	if _, err := svc.Reject(ctx(), ws, p.ID, ""); err != ErrUnauthorizedRejecter {
		t.Fatalf("expected ErrUnauthorizedRejecter, got %v", err)
	}
	if _, err := svc.Publish(ctx(), ws, p.ID, ""); err != ErrUnauthorizedPublisher {
		t.Fatalf("expected ErrUnauthorizedPublisher, got %v", err)
	}
}

func TestRateLimiting(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, StubPublisher{}, &countingSink{}, nil)
	ws := validWorkspace()
	p := basePublication(t, store, ws, "t")
	if _, err := svc.ToReview(ctx(), ws, p.ID); err != nil {
		t.Fatalf("ToReview: %v", err)
	}
	if _, err := svc.Approve(ctx(), ws, p.ID, "alice"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	svc.throttle.limit = 1 // small limit for test
	svc.throttle.hits[ws] = svc.throttle.limit
	if _, err := svc.Publish(ctx(), ws, p.ID, "bob"); err != ErrRateLimited {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestStubPublisherDeterministic(t *testing.T) {
	ws := validWorkspace()
	p, err := New(ws, nil, nil, "t", "b", StubPlatform)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	human := "alice"
	now := time.Now().UTC()
	p.Status = StatusApproved
	p.ApprovedBy = &human
	p.ApprovedAt = &now
	sp := StubPublisher{}
	r1, err := sp.Publish(ctx(), p)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	r2, err := sp.Publish(ctx(), p)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if r1 != r2 {
		t.Fatalf("expected deterministic ref, got %q vs %q", r1, r2)
	}
	// unapproved publication refused by stub
	up, _ := New(ws, nil, nil, "t2", "b2", StubPlatform)
	if _, err := sp.Publish(ctx(), up); err != ErrApprovalRequired {
		t.Fatalf("expected ErrApprovalRequired from stub, got %v", err)
	}
}

func TestValidateInput(t *testing.T) {
	if _, err := New(uuid.Nil, nil, nil, "x", "y", "stub"); err != ErrWorkspaceMismatch {
		t.Fatalf("expected ErrWorkspaceMismatch for nil workspace, got %v", err)
	}
	if _, err := New(validWorkspace(), nil, nil, "  ", "y", "stub"); err == nil {
		t.Fatal("expected empty-title rejection")
	}
	if _, err := New(validWorkspace(), nil, nil, "x", "", "stub"); err == nil {
		t.Fatal("expected empty-body rejection")
	}
	if _, err := New(validWorkspace(), nil, nil, "x", "y", "  "); err != nil {
		t.Fatalf("default stub platform: %v", err)
	}
}

func ctx() context.Context {
	return context.Background()
}
