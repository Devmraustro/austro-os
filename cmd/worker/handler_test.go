package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"austro-os/internal/ai"
	"austro-os/internal/composition"
	"austro-os/internal/event"
	"austro-os/internal/execution"
	"austro-os/internal/worker"

	"github.com/google/uuid"
)

func TestWorkerValidatesPublicationLifecycleEvents(t *testing.T) {
	h := newHandler(&composition.Runtime{})
	workspaceID := uuid.New()
	publicationID := uuid.New()
	details, err := json.Marshal(publicationEventDetails{Event: "publication.failed"})
	if err != nil {
		t.Fatal(err)
	}
	env := &event.UniversalEnvelope{
		WorkspaceID: workspaceID.String(),
		TargetType:  "publication",
		TargetID:    publicationID,
		Details:     details,
		Outcome:     "failure",
	}
	if err := h(env); err != nil {
		t.Fatalf("valid publication lifecycle event should be acknowledged: %v", err)
	}
}

func TestWorkerRejectsMalformedPublicationEvents(t *testing.T) {
	h := newHandler(&composition.Runtime{})
	env := &event.UniversalEnvelope{
		WorkspaceID: uuid.New().String(),
		TargetType:  "publication",
		TargetID:    uuid.New(),
		Details:     json.RawMessage(`{"event":"pipeline.script"}`),
	}
	if err := h(env); err == nil {
		t.Fatal("malformed publication lifecycle target must not be silently acknowledged")
	}
}

// fakeExecStore satisfies the execution ports for the dispatch handler test by
// embedding nil interfaces and overriding only the methods Worker.Run touches
// on the completed path.
type fakeExecStore struct {
	execution.ExecutionStore
	materialized *execution.MaterializedRequest
	completed    bool
}

func (f *fakeExecStore) Materialize(_ context.Context, _, _ uuid.UUID) (*execution.MaterializedRequest, error) {
	return f.materialized, nil
}

func (f *fakeExecStore) Complete(_ context.Context, _ *execution.FencedExecution, _ []byte) error {
	f.completed = true
	return nil
}

func executionDispatchHandler(t *testing.T) func(*event.UniversalEnvelope) error {
	t.Helper()
	store := &fakeExecStore{
		materialized: &execution.MaterializedRequest{
			AIEmployeeName: "test-agent",
			AIEmployeeRole: "assistant",
			Capabilities:   []string{"complete"},
		},
	}
	w := execution.NewWorker(
		store, store, ai.StubProvider{},
		"test-worker",
		60_000_000_000, 20_000_000_000, 50_000_000_000, 2_000_000_000, 120_000_000_000, // 60s/20s/50s/2s/2m
	)
	return newExecutionHandler(w)
}

func testDispatchEnvelope(msg execution.PublishMessage) *event.UniversalEnvelope {
	raw, _ := json.Marshal(msg)
	return &event.UniversalEnvelope{
		EventID:     uuid.New(),
		WorkspaceID: msg.WorkspaceID.String(),
		TargetType:  "execution",
		TargetID:    msg.ExecutionID,
		Details:     raw,
	}
}

// TestExecutionHandlerCompletesDispatch proves a well-formed dispatch lands in
// the worker, the execution completes, and the message is acknowledged.
func TestExecutionHandlerCompletesDispatch(t *testing.T) {
	h := executionDispatchHandler(t)
	msg := execution.PublishMessage{
		WorkspaceID:    uuid.New(),
		ExecutionID:    uuid.New(),
		AIEmployeeID:   uuid.New(),
		RetryID:        uuid.New(),
		Attempt:        1,
		MaxAttempts:    3,
		Generation:     1,
		LeaseExpiresAt: time.Now().Add(time.Minute),
	}
	env := testDispatchEnvelope(msg)
	if err := h(env); err != nil {
		t.Fatalf("valid dispatch should be acknowledged: %v", err)
	}
}

// TestExecutionHandlerRejectsInvalidDispatch proves a dispatch carrying a nil
// identity is a permanent failure, never requeued forever.
func TestExecutionHandlerRejectsInvalidDispatch(t *testing.T) {
	h := executionDispatchHandler(t)
	msg := execution.PublishMessage{
		RetryID: uuid.New(), // nil workspace and execution id
	}
	if err := h(testDispatchEnvelope(msg)); !errors.Is(err, worker.ErrPermanent) {
		t.Fatalf("expected permanent failure for nil identity, got %v", err)
	}
}

// TestExecutionHandlerRejectsUndecodableDispatch proves a dispatch whose Details
// are not a PublishMessage is a permanent failure.
func TestExecutionHandlerRejectsUndecodableDispatch(t *testing.T) {
	h := executionDispatchHandler(t)
	env := &event.UniversalEnvelope{
		WorkspaceID: uuid.New().String(),
		TargetType:  "execution",
		TargetID:    uuid.New(),
		Details:     json.RawMessage(`{"boom":`),
	}
	if err := h(env); !errors.Is(err, worker.ErrPermanent) {
		t.Fatalf("expected permanent failure for undecodable dispatch, got %v", err)
	}
}

// TestExecutionHandlerRejectsForeignTarget proves the execution queue consumer
// refuses messages that are not execution dispatches.
func TestExecutionHandlerRejectsForeignTarget(t *testing.T) {
	h := executionDispatchHandler(t)
	env := &event.UniversalEnvelope{
		WorkspaceID: uuid.New().String(),
		TargetType:  "pipeline",
		TargetID:    uuid.New(),
	}
	if err := h(env); !errors.Is(err, worker.ErrPermanent) {
		t.Fatalf("expected permanent failure for foreign target, got %v", err)
	}
}
