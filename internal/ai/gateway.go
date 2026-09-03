package ai

import (
	"context"
	"fmt"
)

// Gateway is the composed AI entrypoint. It owns the enforcement boundary:
// it requires a non-empty workspace on every call (deny-by-default), consults a
// UsageGuard, delegates to the Provider port, and emits an audit-compatible
// DecisionRecord through the AuditSink. It holds no provider logic itself, so a
// different Provider can be substituted without changing callers (P6, P8).
//
// The Gateway is intentionally not exposed over HTTP in this stage (see
// ADR-004): the proxy surface and its authorization contract are introduced in
// a later step.
type Gateway struct {
	provider Provider
	guard    UsageGuard
	sink     AuditSink
}

// NewGateway wires a Gateway from the injectable ports (config, ports).
func NewGateway(ports ConfigAPI) (*Gateway, error) {
	if ports.Provider == nil {
		return nil, fmt.Errorf("ai: provider is required")
	}
	if ports.Guard == nil {
		return nil, fmt.Errorf("ai: usage guard is required")
	}
	sink := ports.Sink
	if sink == nil {
		sink = NullSink{}
	}
	return &Gateway{provider: ports.Provider, guard: ports.Guard, sink: sink}, nil
}

// Complete runs a completion under the guard and audit boundary.
func (g *Gateway) Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return CompletionResult{}, err
	}
	if ok, err := g.guard.Allow(req.Scope.WorkspaceID, 1); err != nil || !ok {
		if err != nil {
			return CompletionResult{}, err
		}
		return CompletionResult{}, ErrUsageLimitExceeded
	}
	res, err := g.provider.Complete(ctx, req)
	g.emit(req.Scope, completeEvent, outcomeOf(err), "Replaceability")
	return res, err
}

// Embed runs an embedding under the guard and audit boundary.
func (g *Gateway) Embed(ctx context.Context, req EmbedRequest) (EmbeddingResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return EmbeddingResult{}, err
	}
	if ok, err := g.guard.Allow(req.Scope.WorkspaceID, 1); err != nil || !ok {
		if err != nil {
			return EmbeddingResult{}, err
		}
		return EmbeddingResult{}, ErrUsageLimitExceeded
	}
	res, err := g.provider.Embed(ctx, req)
	g.emit(req.Scope, embedEvent, outcomeOf(err), "AI Independence")
	return res, err
}

// Classify runs a classification under the guard and audit boundary.
func (g *Gateway) Classify(ctx context.Context, req ClassificationRequest) (ClassificationResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return ClassificationResult{}, err
	}
	if ok, err := g.guard.Allow(req.Scope.WorkspaceID, 1); err != nil || !ok {
		if err != nil {
			return ClassificationResult{}, err
		}
		return ClassificationResult{}, ErrUsageLimitExceeded
	}
	res, err := g.provider.Classify(ctx, req)
	g.emit(req.Scope, classifyEvent, outcomeOf(err), "Human Oversight")
	return res, err
}

const (
	completeEvent  = "ai.complete"
	embedEvent     = "ai.embed"
	classifyEvent  = "ai.classify"
	outcomeSuccess = "success"
	outcomeFailure = "failed"
)

func outcomeOf(err error) string {
	if err != nil {
		return outcomeFailure
	}
	return outcomeSuccess
}

func (g *Gateway) emit(scope Scope, eventType, outcome, principle string) {
	g.sink.Sink(DecisionRecord{
		EventType:               eventType,
		ConstitutionalPrinciple: principle,
		Outcome:                 outcome,
		WorkspaceID:             scope.WorkspaceID,
		TraceID:                 scope.TraceID,
		SpanID:                  scope.SpanID,
	})
}
