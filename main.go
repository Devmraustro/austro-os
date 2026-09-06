package main

import (
	"context"
	"net/http"
	"os"

	"austro-os/infrastructure/database"
	"austro-os/infrastructure/postgres"
	"austro-os/infrastructure/rabbitmq"
	"austro-os/infrastructure/redis"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/composition"
	"austro-os/internal/config"
	logger "austro-os/internal/log"
	"austro-os/internal/memory"
	"austro-os/internal/middleware"

	"github.com/google/uuid"
)

func main() {
	cfg, err := config.LoadStrict()
	if err != nil {
		logger.NewEntry("invalid-configuration").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	db := database.Initialize(cfg)
	defer db.Close()

	redisClient := redis.Initialize(cfg)
	defer redisClient.Close()

	sink, err := rabbitmq.NewReconnectingSink(cfg.RabbitMQURL, queueFor(cfg))
	if err != nil {
		logger.NewEntry("event-sink-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
	defer sink.Close()

	_ = auth.Initialize(cfg)

	authzService := authz.NewAuthorizer()

	// The API composes the full runtime with the production adapters: PostgreSQL
	// stores, Redis memory, RabbitMQ event/audit sinks. This proves the wiring
	// and keeps backend selection honest on a live process.
	rt, err := composition.Compose(cfg, composition.Stores{
		Publications: postgres.NewPublicationStore(db),
		Pipelines:    postgres.NewPipelineStore(db),
	}, composition.Sinks{
		AIDecision:    rabbitmq.NewDecisionSink(sink),
		PublishAudit:  rabbitmq.NewPublishLogAuditSink(),
		PipelineAudit: rabbitmq.NewPipelineLogAuditSink(),
		PublishEvent:  rabbitmq.NewPublishEventSink(sink).Publish,
		PipelineEvent: rabbitmq.NewPipelineEventSink(sink).PublishPipeline,
	})
	if err != nil {
		logger.NewEntry("runtime-compose-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
	_ = memory.NewBank(redis.NewMemoryStore(redisClient), logMemoryAudit{}, logMemoryEvents{})

	logger.NewEntry("austro-os-startup").
		With("version", "1.0").
		With("environment", cfg.Environment).
		With("composition", "ok").
		With("ai_backend", rt.AIBackend).
		With("publish_backend", rt.PublishBackend).
		With("usage_limit_per_workspace", cfg.AIUsageLimitPerWorkspace).
		Log()

	mux := http.NewServeMux()

	mux.HandleFunc("/health/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ready":true}`))
	})

	// Deny-by-default: every protected route must carry an explicit allow rule.
	// Any request without an explicit permission for its action/resource is DENIED.
	protected := authzMiddleware(authzService, mux)

	logger.NewEntry("austro-os-serving").With("address", cfg.ServerAddress).Log()
	if err := http.ListenAndServe(cfg.ServerAddress, middleware.Middleware(protected)); err != nil {
		logger.NewEntry("http-server-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
}

// queueFor mirrors rabbitmq.Initialize: the configured queue name, or the
// default austro.events when unset.
func queueFor(cfg *config.Config) string {
	if cfg.RabbitMQQueue != "" {
		return cfg.RabbitMQQueue
	}
	return "austro.events"
}

// authzMiddleware wraps the mux with the deny-by-default authorization
// enforcement point. Rules are explicit; there is no permissive fallback.
func authzMiddleware(a *authz.Authorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Method
		resource := r.URL.Path

		// Health/liveness probes are public per OpenAPI contracts.
		if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
			next.ServeHTTP(w, r)
			return
		}

		if err := a.Authorize(r, action, resource); err != nil {
			logger.NewEntry("authorization-denied").
				With("action", action).
				With("resource", resource).
				With("constitutional_principle", "Security by Design").
				Log()
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type logMemoryAudit struct{}

func (logMemoryAudit) Record(_ context.Context, rec memory.AuditRecord) {
	logger.NewEntry("memory-audit").
		With("event", rec.EventType).
		With("principle", rec.ConstitutionalPrinciple).
		With("outcome", rec.Outcome).
		With("workspace_id", rec.WorkspaceID).
		With("layer", rec.Layer).
		With("key", rec.Key).
		Log()
}

type logMemoryEvents struct{}

func (logMemoryEvents) Publish(_ context.Context, eventType string, workspaceID uuid.UUID, layer, key, traceID, spanID string) error {
	logger.NewEntry("memory-event").
		With("event", eventType).
		With("workspace_id", workspaceID).
		With("layer", layer).
		With("key", key).
		Log()
	return nil
}
