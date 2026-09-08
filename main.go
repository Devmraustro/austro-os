package main

import (
	"context"
	"net/http"
	"os"

	"austro-os/infrastructure/database"
	"austro-os/infrastructure/postgres"
	"austro-os/infrastructure/rabbitmq"
	"austro-os/infrastructure/redis"
	"austro-os/internal/api"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/composition"
	"austro-os/internal/config"
	logger "austro-os/internal/log"
	"austro-os/internal/memory"
	"austro-os/internal/middleware"
	"austro-os/internal/rbac"

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

	jwtSvc := auth.Initialize(cfg)

	authzService := authz.NewAuthorizer()

	authHandler := api.NewAuthHandler(cfg, jwtSvc, postgres.NewUserStore(db))

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

	// Authentication endpoints. Login/refresh/logout/bootstrap are explicit
	// public exemptions (unauthenticated by design); every other route requires
	// an explicit authorization rule.
	mux.HandleFunc("POST /api/auth/bootstrap", authHandler.Bootstrap)
	mux.HandleFunc("POST /api/auth/login", authHandler.Login)
	mux.HandleFunc("POST /api/auth/refresh", authHandler.Refresh)
	mux.HandleFunc("POST /api/auth/logout", authHandler.Logout)
	mux.HandleFunc("GET /api/me", authHandler.Me)

	// Workspace administration. All three routes are protected (no public
	// exemption), so they are reachable only through an explicit allow rule:
	// GET/POST /workspaces are founder organization-level (ScopeNone), and
	// GET /workspaces/{id} is founder org-level or the workspace admin's own
	// workspace (ScopePath). The handlers never accept a client-supplied
	// workspace: the lookup is bound to the verified claims.
	workspaceHandler := api.NewWorkspaceHandler(postgres.NewWorkspaceStore(db))
	mux.HandleFunc("GET /workspaces", workspaceHandler.List)
	mux.HandleFunc("POST /workspaces", workspaceHandler.Create)
	mux.HandleFunc("GET /workspaces/{id}", workspaceHandler.Get)

	// Deny-by-default: every protected route must carry an explicit allow rule.
	// Any request without an explicit permission for its action/resource is DENIED.
	// The RBAC contract is documented in internal/rbac; only the rules for
	// routes actually registered by this server are seeded — a route declared
	// in the contract but not registered stays denied.
	authzService.AddRules(rbac.ImplementedRules())
	protected := authHandler.RequireAuth(authzMiddleware(authzService, mux))

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

// isPublicEndpoint enumerates the explicit public exemptions. They are the
// health probes and the unauthenticated authentication entry points. Everything
// else must satisfy an explicit authorization rule.
func isPublicEndpoint(method, path string) bool {
	switch {
	case (method == http.MethodGet || method == http.MethodHead) &&
		(path == "/health/live" || path == "/health/ready"):
		return true
	case method == http.MethodPost &&
		(path == "/api/auth/login" ||
			path == "/api/auth/refresh" ||
			path == "/api/auth/logout" ||
			path == "/api/auth/bootstrap"):
		return true
	}
	return false
}

// authzMiddleware wraps the mux with the deny-by-default authorization
// enforcement point. Rules are explicit; there is no permissive fallback. The
// auth handler's RequireAuth middleware runs first (it activates before this
// layer) so verified claims are present on the request context when an action
// is authorized.
func authzMiddleware(a *authz.Authorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Method
		resource := r.URL.Path

		// Explicit public endpoints (health probes and unauthenticated auth
		// entry points) are exempt from the authorization layer by contract.
		if isPublicEndpoint(action, resource) {
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
