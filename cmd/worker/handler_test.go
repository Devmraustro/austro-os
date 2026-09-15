package main

import (
	"encoding/json"
	"testing"

	"austro-os/internal/composition"
	"austro-os/internal/event"

	"github.com/google/uuid"
)

func TestWorkerValidatesPublicationLifecycleEvents(t *testing.T) {
	h := newHandler(&composition.Runtime{})
	workspaceID := uuid.New()
	publicationID := uuid.New()
	details, err := json.Marshal(publicationEventDetails{Event: "publication.failed"})
	if err != nil { t.Fatal(err) }
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
