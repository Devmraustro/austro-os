// Package ai provides the provider-agnostic AI gateway for AUSTRO OS.
//
// It defines a small, replaceable backend (Provider), a configurable
// usage/rate/cost guard, and an audit-compatible decision sink. The package is
// intentionally free of any real AI provider or infrastructure import:
// dependency direction is preserved (shared services may use it, it must not
// depend on infrastructure). Callers obtain AI behavior through the Provider
// port, so a different implementation can be substituted without changing
// callers (Principles 6 and 8).
package ai

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

// Capability identifies one of the gateway operations the platform relies on.
type Capability string

const (
	CapabilityComplete Capability = "complete"
	CapabilityEmbed    Capability = "embed"
	CapabilityClassify Capability = "classify"
)

// Scope carries the required context for an AI operation. WorkspaceID must be
// non-empty at the gateway boundary (deny-by-default isolation). TraceID and
// SpanID propagate existing observability correlation where applicable.
type Scope struct {
	WorkspaceID string    `json:"workspace_id"`
	TraceID     uuid.UUID `json:"trace_id,omitempty"`
	SpanID      uuid.UUID `json:"span_id,omitempty"`
}

// CompletionRequest asks the backend to produce a text completion for the given
// instruction under the given scope.
type CompletionRequest struct {
	Scope       Scope  `json:"scope"`
	Instruction string `json:"instruction"`
}

// CompletionResult is the deterministic backend output for a completion.
type CompletionResult struct {
	Text string `json:"text"`
}

// EmbedRequest asks the backend to embed text into a fixed-size vector.
type EmbedRequest struct {
	Scope      Scope  `json:"scope"`
	Content    string `json:"content"`
	Dimensions int    `json:"dimensions"`
}

// EmbeddingResult carries the produced vector and its length.
type EmbeddingResult struct {
	Values     []float32 `json:"values"`
	Dimensions int       `json:"dimensions"`
}

// ClassificationRequest asks the backend to assign a label/text category to the
// provided input.
type ClassificationRequest struct {
	Scope      Scope    `json:"scope"`
	Categories []string `json:"categories"`
	Input      string   `json:"input"`
}

// ClassificationResult is the deterministic backend category verdict.
type ClassificationResult struct {
	Category string `json:"category"`
}

// Provider is the replaceable AI backend port. Every method requires a Scope
// with a non-empty WorkspaceID. Implementations must be stateless or otherwise
// never mix data across distinct WorkspaceIDs (no cross-workspace leakage).
type Provider interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResult, error)
	Embed(ctx context.Context, req EmbedRequest) (EmbeddingResult, error)
	Classify(ctx context.Context, req ClassificationRequest) (ClassificationResult, error)
}

// UsageGuard establishes the abstraction for configurable usage/rate/cost
// guarding. It is invoked at the gateway boundary before delegating to the
// Provider. A guard that observes a limit breach returns a non-nil error and
// the operation is refused. There is no provider billing behavior here; limits
// are expressed as plain integer budgets so the mechanism is testable without
// any external provider.
type UsageGuard interface {
	// Allow reports whether an operation with the given cost unit ("1") is
	// permitted for the workspace, and records it if allowed. Implementations
	// may track an aggregate counter across workspaces when allowed is nil.
	Allow(workspaceID string, unit uint64) (bool, error)
}

// AuditSink is the audit-compatible decision sink. A concrete integration that
// appends to the Phase 1 audit hash chain supplies a sink implementing this
// port; the gateway never couples to that storage implementation directly.
type AuditSink interface {
	// Sink records a decision. Payload carries only non-secret metadata;
	// it is the sink's responsibility to persist it through the audit model.
	Sink(decision DecisionRecord)
}

// DecisionRecord is the gateway's audit contract. Its shape mirrors the
// existing audit model (event type, constitutional principle, outcome, trace,
// span, workspace) without replacing the Phase 1 hash chain.
type DecisionRecord struct {
	EventType               string    `json:"event_type"`
	ConstitutionalPrinciple string    `json:"constitutional_principle"`
	Outcome                 string    `json:"outcome"`
	WorkspaceID             string    `json:"workspace_id"`
	TraceID                 uuid.UUID `json:"trace_id,omitempty"`
	SpanID                  uuid.UUID `json:"span_id,omitempty"`
}

// Sentinel errors returned by the gateway boundary.
var (
	ErrWorkspaceRequired  = errors.New("ai: workspace id is required")
	ErrInvalidInput       = errors.New("ai: invalid input")
	ErrUsageLimitExceeded = errors.New("ai: usage limit exceeded")
)

// NullSink discards decisions. It is the default when no sink is injected.
type NullSink struct{}

// Sink implements AuditSink by ignoring the decision.
func (NullSink) Sink(_ DecisionRecord) {}

// NoopGuard is an unlimited UsageGuard for contexts where guarding is not yet
// configured. It always allows and records nothing.
type NoopGuard struct{}

// Allow always permits the operation.
func (NoopGuard) Allow(_ string, _ uint64) (bool, error) {
	return true, nil
}

// Config covers the limits applied by ConfigurableUsageGuard.
type Config struct {
	// MaxPerWorkspace is the maximum total operations allowed per workspace
	// within the Guard's lifetime. Zero (or config with no limit set) gives the
	// guard no upper bound; the model is intentionally simple and billing-free.
	MaxPerWorkspace uint64
}

// ConfigurableUsageGuard is a deterministic, in-memory UsageGuard. It enforces
// an optional per-workspace operation budget and is safe for concurrent use.
// It does not perform any cost accounting or provider billing — it only refuses
// operations once a configured budget for a workspace is spent.
type ConfigurableUsageGuard struct {
	mu   sync.Mutex
	cfg  Config
	used map[string]uint64
}

// NewConfigurableUsageGuard returns a guard honouring cfg.
func NewConfigurableUsageGuard(cfg Config) *ConfigurableUsageGuard {
	return &ConfigurableUsageGuard{
		cfg:  cfg,
		used: make(map[string]uint64),
	}
}

// Allow permits the operation when the workspace budget has not been capped.
func (g *ConfigurableUsageGuard) Allow(workspaceID string, unit uint64) (bool, error) {
	if workspaceID == "" {
		return false, nil
	}
	if g.cfg.MaxPerWorkspace == 0 {
		// No ceiling configured: the guard is effectively unlimited but still
		// records usage so callers can observe it deterministically.
		return true, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	next := g.used[workspaceID] + unit
	if next > g.cfg.MaxPerWorkspace {
		return false, nil
	}
	g.used[workspaceID] = next
	return true, nil
}

// Used returns the number of units recorded for a workspace (0 if none).
func (g *ConfigurableUsageGuard) Used(workspaceID string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used[workspaceID]
}

// ConfigAPI aggregates the injectable ports a Gateway is born with.
type ConfigAPI struct {
	Provider Provider
	Guard    UsageGuard
	Sink     AuditSink
}
