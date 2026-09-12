package task

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// memStore is an in-memory, workspace-isolated TaskStore used exclusively for
// deterministic domain tests. A task is only reachable via its owning
// workspace, mirroring RLS behavior.
type memStore struct {
	mu    sync.Mutex
	items map[uuid.UUID]*Task
}

func newMemStore() *memStore {
	return &memStore{items: map[uuid.UUID]*Task{}}
}

func (m *memStore) Create(_ context.Context, t *Task) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.items[t.ID] = &cp
	return &cp, nil
}

func (m *memStore) Get(_ context.Context, workspaceID, id uuid.UUID) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.items[id]
	if !ok {
		return nil, ErrNotFound
	}
	if t.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	cp := *t
	return &cp, nil
}

func (m *memStore) List(_ context.Context, workspaceID uuid.UUID, status *Status) ([]*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*Task{}
	for _, t := range m.items {
		if t.WorkspaceID != workspaceID {
			continue
		}
		if status != nil && t.Status != *status {
			continue
		}
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memStore) Update(_ context.Context, workspaceID uuid.UUID, t *Task) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.items[t.ID]
	if !ok {
		return nil, ErrNotFound
	}
	if existing.WorkspaceID != workspaceID || t.WorkspaceID != workspaceID {
		return nil, ErrWorkspaceMismatch
	}
	cp := *t
	cp.UpdatedAt = time.Unix(0, 1).UTC()
	m.items[t.ID] = &cp
	return &cp, nil
}

func (m *memStore) Delete(_ context.Context, workspaceID, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.items[id]
	if !ok {
		return ErrNotFound
	}
	if t.WorkspaceID != workspaceID {
		return ErrWorkspaceMismatch
	}
	delete(m.items, id)
	return nil
}

type recordingAudit struct {
	mu   sync.Mutex
	recs []AuditRecord
}

func (r *recordingAudit) Record(_ context.Context, rec AuditRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
}

func (r *recordingAudit) count(eventType, outcome string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.recs {
		if rec.EventType == eventType && rec.Outcome == outcome {
			n++
		}
	}
	return n
}

func TestValidStatusAndPriority(t *testing.T) {
	if !ValidStatus(StatusBacklog) || !ValidStatus(StatusCompleted) {
		t.Error("expected backlog/completed valid")
	}
	if ValidStatus(Status("bogus")) {
		t.Error("bogus status must be invalid")
	}
	if !ValidPriority(PriorityUrgent) || ValidPriority(Priority("bogus")) {
		t.Error("priority validation broken")
	}
	if !ValidAssigneeType(AssigneeAI) || ValidAssigneeType(AssigneeType("x")) {
		t.Error("assignee type validation broken")
	}
}

func TestNewTask(t *testing.T) {
	ws := uuid.New()
	tk, err := New(ws, "draft script", PriorityHigh, "desc")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != StatusBacklog {
		t.Error("expected initial backlog status")
	}
	if tk.WorkspaceID != ws {
		t.Error("workspace not set")
	}
	if _, err := New(uuid.Nil, "x", PriorityNormal, ""); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Error("expected workspace mismatch on nil workspace")
	}
	if _, err := New(ws, "", PriorityNormal, ""); !errors.Is(err, ErrInvalidInput) {
		t.Error("expected invalid input on empty title")
	}
	if _, err := New(ws, "x", Priority("bogus"), ""); !errors.Is(err, ErrPriority) {
		t.Error("expected priority error")
	}
}

func TestLifecycleTransitions(t *testing.T) {
	cases := []struct {
		from, to Status
		ok       bool
	}{
		{StatusBacklog, StatusPlanned, true},
		{StatusBacklog, StatusCancelled, true},
		{StatusPlanned, StatusInProgress, true},
		{StatusInProgress, StatusInReview, true},
		{StatusInReview, StatusCompleted, true},
		{StatusInReview, StatusRejected, true},
		// Invalid
		{StatusBacklog, StatusCompleted, false},
		{StatusPlanned, StatusInReview, false},
		{StatusInProgress, StatusCompleted, false},
		// Terminal has no outgoing
		{StatusCompleted, StatusBacklog, false},
		{StatusCancelled, StatusInProgress, false},
		{StatusFailed, StatusBacklog, false},
		{StatusRejected, StatusBacklog, false},
		// Same status
		{StatusBacklog, StatusBacklog, false},
	}
	for _, c := range cases {
		err := CanTransition(c.from, c.to)
		if c.ok && err != nil {
			t.Errorf("%s -> %s: expected ok, got %v", c.from, c.to, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s -> %s: expected error", c.from, c.to)
		}
	}
}

func TestServiceLifecycleEndToEnd(t *testing.T) {
	s := NewService(newMemStore(), &recordingAudit{}, nil)
	ws := uuid.New()
	tk, err := s.Create(context.Background(), ws, "write script", PriorityNormal, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Transition(context.Background(), ws, tk.ID, StatusPlanned); err != nil {
		t.Fatalf("planned: %v", err)
	}
	if _, err := s.Transition(context.Background(), ws, tk.ID, StatusInProgress); err != nil {
		t.Fatalf("in_progress: %v", err)
	}
	if _, err := s.Transition(context.Background(), ws, tk.ID, StatusInReview); err != nil {
		t.Fatalf("in_review: %v", err)
	}
	done, err := s.Transition(context.Background(), ws, tk.ID, StatusCompleted)
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if done.Status != StatusCompleted {
		t.Error("task not completed")
	}

	// Terminal: no further transition.
	if _, err := s.Transition(context.Background(), ws, tk.ID, StatusBacklog); !errors.Is(err, ErrTerminalState) {
		t.Errorf("expected terminal state error, got %v", err)
	}
}

func TestServiceRejectsInvalidTransition(t *testing.T) {
	audit := &recordingAudit{}
	s := NewService(newMemStore(), audit, nil)
	ws := uuid.New()
	tk, err := s.Create(context.Background(), ws, "x", PriorityNormal, "")
	if err != nil {
		t.Fatal(err)
	}
	// backlog -> completed is invalid.
	_, err = s.Transition(context.Background(), ws, tk.ID, StatusCompleted)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected invalid transition, got %v", err)
	}
	if audit.count("task.transition", "failed") != 1 {
		t.Error("expected a failed transition audit record")
	}
}

func TestServiceWorkspaceIsolation(t *testing.T) {
	s := NewService(newMemStore(), nil, nil)
	wsA := uuid.New()
	wsB := uuid.New()
	tk, err := s.Create(context.Background(), wsA, "secret plan", PriorityHigh, "")
	if err != nil {
		t.Fatal(err)
	}
	// Cross-workspace read must fail.
	if _, err := s.Get(context.Background(), wsB, tk.ID); !errors.Is(err, ErrWorkspaceMismatch) && !errors.Is(err, ErrNotFound) {
		t.Errorf("expected cross-workspace denial, got %v", err)
	}
	// Cross-workspace transition must fail.
	if _, err := s.Transition(context.Background(), wsB, tk.ID, StatusPlanned); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Errorf("expected workspace mismatch on cross-workspace transition, got %v", err)
	}
	// Own-workspace list returns only its tasks.
	_, err = s.Create(context.Background(), wsB, "b task", PriorityLow, "")
	if err != nil {
		t.Fatal(err)
	}
	listA, _ := s.List(context.Background(), wsA, nil)
	if len(listA) != 1 || listA[0].ID != tk.ID {
		t.Errorf("workspace A list must contain only its task; got %d", len(listA))
	}
}

func TestServiceTracePropagation(t *testing.T) {
	audit := &recordingAudit{}
	s := NewService(newMemStore(), audit, nil)
	ws := uuid.New()
	ctx := WithTrace(context.Background(), "trace-1", "span-1")
	tk, err := s.Create(ctx, ws, "x", PriorityNormal, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Transition(ctx, ws, tk.ID, StatusPlanned)
	if err != nil {
		t.Fatal(err)
	}
	audit.mu.Lock()
	found := false
	for _, rec := range audit.recs {
		if rec.EventType == "task.transition" && rec.TraceID == "trace-1" && rec.SpanID == "span-1" {
			found = true
		}
	}
	audit.mu.Unlock()
	if !found {
		t.Error("transition audit record did not propagate trace/span ids")
	}
}

func TestServiceListByStatus(t *testing.T) {
	s := NewService(newMemStore(), nil, nil)
	ws := uuid.New()
	a, _ := s.Create(context.Background(), ws, "a", PriorityNormal, "")
	if _, err := s.Transition(context.Background(), ws, a.ID, StatusPlanned); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Create(context.Background(), ws, "b", PriorityNormal, "")
	backlog := StatusBacklog
	list, err := s.List(context.Background(), ws, &backlog)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "b" {
		t.Errorf("expected exactly one backlog task (b); got %d: %v", len(list), list)
	}
}
