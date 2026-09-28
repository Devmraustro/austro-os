package execution

import (
	"context"
	"log/slog"
)

// StubPublisher is the deterministic, offline dispatch transport (the default
// ExecutionTransportBackend). It logs each fenced dispatch and reports success
// without touching a broker, so the retry is marked dispatched and is left for
// the lease to expire and recovery to reschedule — the same offline behaviour
// the AI and publishing stub adapters provide. It exists so the full
// dispatcher pipeline runs, compose-wired and testable, without any external
// service.
type StubPublisher struct {
	log *slog.Logger
}

// NewStubPublisher returns a stub transport logging through slog.Default().
func NewStubPublisher() *StubPublisher {
	return &StubPublisher{log: slog.Default()}
}

// Publish records the fenced dispatch and returns nil. It is at-least-once by
// the caller's contract: the fenced RecordDispatch step that follows makes a
// redundant success harmless.
func (p *StubPublisher) Publish(_ context.Context, msg PublishMessage) error {
	p.log.Info("execution dispatch",
		"workspace", msg.WorkspaceID,
		"execution", msg.ExecutionID,
		"retry", msg.RetryID,
		"attempt", msg.Attempt,
		"max_attempts", msg.MaxAttempts,
		"generation", msg.Generation,
		"backend", "stub",
	)
	return nil
}
