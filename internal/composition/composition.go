// Package composition assembles the process-scoped application runtime for the
// API and worker entrypoints. It imports only shared/internal packages (never
// infrastructure; criterion #32) and takes persistence ports, sinks and
// configuration as inputs, so every adapter decision is observable and
// testable without a container. Entrypoints satisfy the ports with
// infrastructure adapters and call Compose to obtain the wired services.
package composition

import (
	"context"
	"fmt"
	"time"

	"austro-os/internal/ai"
	"austro-os/internal/config"
	"austro-os/internal/orchestration"
	"austro-os/internal/publish"

	"github.com/google/uuid"
)

// Stores bundles the persistence ports consumed by the composed services. The
// entrypoints satisfy them with infrastructure adapters.
type Stores struct {
	// Publications is the publication persistence port.
	Publications publish.PublicationStore
	// Pipelines is the creator pipeline persistence port.
	Pipelines orchestration.PipelineStore
}

// Sinks carries the observability/event output ports that the composition
// binds to the domain services. Adapters are supplied by the entrypoints
// (e.g. a RabbitMQ publisher); nil fields default to safe no-ops.
type Sinks struct {
	// AIDecision records AI gateway decisions (non-secret metadata).
	AIDecision ai.AuditSink
	// PublishAudit records publishing-service decisions.
	PublishAudit publish.AuditSink
	// PipelineAudit records orchestration decisions.
	PipelineAudit orchestration.AuditSink
	// PublishEvent emits a publication lifecycle event (publication.created,
	// publication.approved, publication.published, ...).
	PublishEvent func(ctx context.Context, eventType string, publicationID, workspaceID uuid.UUID, traceID, spanID string) error
	// PipelineEvent emits a pipeline lifecycle event (pipeline.research,
	// pipeline.script, ...) that drives the worker advance loop.
	PipelineEvent func(ctx context.Context, eventType string, pipelineID, workspaceID uuid.UUID, stage, traceID, spanID string) error
}

// Runtime is the composed application runtime for a process.
type Runtime struct {
	// Config is the strict configuration the runtime was built from.
	Config *config.Config

	// AI is the enforcement boundary for AI operations (usage budget, scope,
	// audit). All AI calls flow through it.
	AI *ai.Gateway
	// AIProvider is the selected provider adapter (observable for ops/tests).
	AIProvider ai.Provider
	// AIBackend is the resolved provider selector ("stub" or a real backend).
	AIBackend string
	// UsageGuard is the configured per-workspace usage budget.
	UsageGuard ai.UsageGuard

	// Publish is the publishing application service.
	Publish *publish.Service
	// Publisher is the selected publisher adapter.
	Publisher publish.Publisher
	// PublishBackend is the resolved publisher selector.
	PublishBackend string

	// Orchestration is the creator pipeline service.
	Orchestration *orchestration.Service
	// Handler is the worker glue that advances pipelines from events.
	Handler *orchestration.Handler
}

// Compose builds the runtime from the strict config and injected ports. It
// fails closed: an unknown or unbuildable adapter selection is an error rather
// than a silent fallback. Config.Validate must already have passed; the
// runtime entrypoints use config.LoadStrict.
func Compose(cfg *config.Config, stores Stores, sinks Sinks) (*Runtime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("composition: config is required")
	}
	aiBackend := cfg.AIBackend
	if aiBackend == "" {
		aiBackend = config.AIBackendStub
	}
	publishBackend := cfg.PublishBackend
	if publishBackend == "" {
		publishBackend = config.PublishBackendStub
	}

	// AI gateway: provider selected by AIBackend, per-workspace budget from the
	// strict config, audit decisions delegated to the injected sink.
	provider, err := ai.NewProvider(ai.ProviderConfig{
		Backend: cfg.AIBackend,
		Model:   cfg.AIModel,
		BaseURL: cfg.AIBaseURL,
		APIKey:  cfg.AIAPIKey,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("composition: ai provider: %w", err)
	}
	guard := ai.NewConfigurableUsageGuard(ai.Config{MaxPerWorkspace: cfg.AIUsageLimitPerWorkspace})
	if sinks.AIDecision == nil {
		sinks.AIDecision = ai.NullSink{}
	}
	gw, err := ai.NewGateway(ai.ConfigAPI{Provider: provider, Guard: guard, Sink: sinks.AIDecision})
	if err != nil {
		return nil, fmt.Errorf("composition: ai gateway: %w", err)
	}

	// Publishing service: publisher selected by PublishBackend.
	publisher, err := publish.NewPublisher(publish.PublisherConfig{
		Backend:             cfg.PublishBackend,
		WebhookURL:          cfg.PublishWebhookURL,
		Token:               cfg.PublishToken,
		IdempotencyKeyField: cfg.PublishIdempotencyField,
		MaxAttempts:         cfg.PublishMaxAttempts,
		BackoffBase:         cfg.PublishRetryBackoffBase,
		BackoffMax:          cfg.PublishRetryBackoffMax,
		Timeout:             30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("composition: publisher: %w", err)
	}
	if sinks.PublishAudit == nil {
		sinks.PublishAudit = publish.NullAuditSink{}
	}
	publishSvc := publish.NewService(stores.Publications, publisher, sinks.PublishAudit, publishEventSink{fn: sinks.PublishEvent})

	// Creator pipeline service. Stage capability ports default to the
	// deterministic stubs (ADR-010); the review preparation step uses the AI
	// gateway when a real AI backend is selected, so AI usage enforcement runs
	// on a live runtime call path.
	if sinks.PipelineAudit == nil {
		sinks.PipelineAudit = orchestration.NullAuditSink{}
	}
	orchSvc := orchestration.NewService(
		stores.Pipelines,
		orchestration.StubResearcher{},
		orchestration.StubScriptWriter{},
		reviewerFor(gw, cfg.AIBackend),
		orchestration.StubPublisher{},
		sinks.PipelineAudit,
		pipelineEventSink{fn: sinks.PipelineEvent},
	)
	// Creator's publish handoff reuses the existing Publishing aggregate and
	// service. No second publication engine is created in orchestration.
	orchSvc.SetPublicationPort(pipelinePublicationAdapter{svc: publishSvc})

	return &Runtime{
		Config:         cfg,
		AI:             gw,
		AIProvider:     provider,
		AIBackend:      aiBackend,
		UsageGuard:     guard,
		Publish:        publishSvc,
		Publisher:      publisher,
		PublishBackend: publishBackend,
		Orchestration:  orchSvc,
		Handler:        orchestration.NewHandler(orchSvc),
	}, nil
}

// publishEventSink adapts a closure (or a no-op) to the publishing EventSink port.
type publishEventSink struct {
	fn func(ctx context.Context, eventType string, publicationID, workspaceID uuid.UUID, traceID, spanID string) error
}

func (s publishEventSink) Publish(ctx context.Context, eventType string, publicationID, workspaceID uuid.UUID, traceID, spanID string) error {
	if s.fn == nil {
		return nil
	}
	return s.fn(ctx, eventType, publicationID, workspaceID, traceID, spanID)
}

// pipelineEventSink adapts a closure (or a no-op) to the orchestration EventSink port.
type pipelineEventSink struct {
	fn func(ctx context.Context, eventType string, pipelineID, workspaceID uuid.UUID, stage, traceID, spanID string) error
}

func (s pipelineEventSink) PublishPipeline(ctx context.Context, eventType string, pipelineID, workspaceID uuid.UUID, stage, traceID, spanID string) error {
	if s.fn == nil {
		return nil
	}
	return s.fn(ctx, eventType, pipelineID, workspaceID, stage, traceID, spanID)
}
