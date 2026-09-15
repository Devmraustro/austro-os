package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"austro-os/infrastructure/auditstore"
	"austro-os/infrastructure/database"
	"austro-os/infrastructure/postgres"
	"austro-os/infrastructure/rabbitmq"
	"austro-os/infrastructure/redis"
	"austro-os/internal/ai"
	"austro-os/internal/api"
	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/composition"
	"austro-os/internal/config"
	"austro-os/internal/knowledge"
	logger "austro-os/internal/log"
	"austro-os/internal/memory"
	"austro-os/internal/middleware"
	"austro-os/internal/rbac"
	"austro-os/internal/task"
	"austro-os/internal/webui"

	"github.com/google/uuid"
)

// shutdownDrainTimeout bounds how long graceful shutdown waits for in-flight
// requests. It is comfortably above the 60s write timeout so a request that is
// still progressing is allowed to finish, while guaranteeing the process exits
// rather than hanging on a stalled client.
const shutdownDrainTimeout = 75 * time.Second

func main() {
	cfg, err := config.LoadStrict()
	if err != nil {
		logger.NewEntry("invalid-configuration").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	// Bootstrap the schema as the owner principal, then serve on the
	// unprivileged runtime pool. MustInitializeTopology verifies that pool
	// against the live database before returning, so a process that reaches
	// this line is running as a role the workspace policies actually constrain.
	handles := database.MustInitializeTopology(cfg)
	db := handles.Runtime
	defer db.Close()
	defer handles.Admin.Close()

	// Persistent audit writer. It runs on the administrative handle because
	// the runtime role is deliberately append-only on audit_events and cannot
	// read back the chain it writes. A failure here is fatal: the security
	// model requires durable audit evidence, so starting without it would let
	// every audited operation report a record that was never written.
	auditCtx, auditCancel := context.WithTimeout(context.Background(), 30*time.Second)
	auditStore, err := auditstore.New(auditCtx, handles.Admin)
	auditCancel()
	if err != nil {
		logger.NewEntry("audit-store-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

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

	authHandler := api.NewAuthHandler(cfg, jwtSvc, postgres.NewUserStore(db)).SetAuditSink(auditStore)

	// The API composes the full runtime with the production adapters: PostgreSQL
	// stores, Redis memory, RabbitMQ event/audit sinks. This proves the wiring
	// and keeps backend selection honest on a live process.
	rt, err := composition.Compose(cfg, composition.Stores{
		Publications: postgres.NewPublicationStore(db),
		Pipelines:    postgres.NewPipelineStore(db),
	}, composition.Sinks{
		AIDecision:    rabbitmq.NewDecisionSink(sink),
		PublishAudit:  auditstore.NewPublishSink(auditStore),
		PipelineAudit: auditstore.NewPipelineSink(auditStore),
		PublishEvent:  rabbitmq.NewPublishEventSink(sink).Publish,
		PipelineEvent: rabbitmq.NewPipelineEventSink(sink).PublishPipeline,
	})
	if err != nil {
		logger.NewEntry("runtime-compose-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
	memoryBank := memory.NewBank(
		redis.NewMemoryStore(redisClient),
		auditstore.NewMemorySink(auditStore),
		logMemoryEvents{},
	)
	memoryHandler := api.NewMemoryHandler(memoryBank).SetAuditSink(auditStore)
	publicationHandler := api.NewPublicationHandler(rt.Publish)

	logger.NewEntry("austro-os-startup").
		With("version", "1.0").
		With("environment", cfg.Environment).
		With("composition", "ok").
		With("ai_backend", rt.AIBackend).
		With("publish_backend", rt.PublishBackend).
		With("usage_limit_per_workspace", cfg.AIUsageLimitPerWorkspace).
		Log()

	// Workspaces are organization-level records, so the store runs on the admin
	// handle rather than the runtime one. The runtime role is deliberately not
	// the table owner, and an unbound runtime session is visible to no
	// workspace at all, so organization-level listing and creation have to go
	// through the principal whose policy grants that reach. The admin role is
	// not a superuser and has no BYPASSRLS: its reach over this table is
	// exactly org_admin_policy, and every tenant-scoped table stays on the
	// runtime handle.
	workspaceHandler := api.NewWorkspaceHandler(postgres.NewWorkspaceStore(handles.Admin)).SetAuditSink(auditStore)

	// Audit visibility runs on two deliberately different principals. The
	// organization-wide reads need audit_org_policy, which names only the
	// administrative role; the workspace-scoped read uses the unprivileged
	// runtime pool so audit_workspace_policy confines it inside PostgreSQL
	// rather than relying on a WHERE clause in Go. Passing the same handle to
	// both would quietly make every workspace read organization-wide.
	auditHandler := api.NewAuditHandler(
		auditstore.NewReader(handles.Admin),
		auditstore.NewReader(db),
	)

	// Task management. The store runs on the unprivileged runtime handle, which
	// is the point: every operation binds app.current_workspace inside its own
	// transaction and workspace_isolation_policy does the confining, so a task
	// belonging to another tenant is not merely filtered out in Go but invisible
	// to the query.
	//
	// Audit is recorded by the handler rather than by the domain service, because
	// the handler is where the verified caller is known. The service's own
	// AuditSink hardcodes a "system" actor, which would produce a trail that
	// cannot say who moved the work. Its event sink stays a no-op: nothing
	// consumes task events today, and publishing to a queue with no consumer
	// would be unverifiable surface rather than a feature.
	// Knowledge. The embedder comes from the configured AI provider, which is the
	// deterministic stub unless a backend is selected; ai.NewProvider fails fast on
	// an unknown one rather than silently degrading. The store runs on the
	// unprivileged runtime handle, not the admin handle, so the workspace policy
	// genuinely constrains it.
	aiProvider, err := ai.NewProvider(ai.ProviderConfig{
		Backend: cfg.AIBackend,
		Model:   cfg.AIModel,
		BaseURL: cfg.AIBaseURL,
		APIKey:  cfg.AIAPIKey,
	})
	if err != nil {
		logger.NewEntry("ai-provider-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
	knowledgeStore := postgres.NewKnowledgeStore(db)
	knowledgeService := knowledge.NewService(knowledgeStore,
		knowledge.NewGatewayEmbedder(aiProvider), nil, nil, knowledge.DefaultEmbeddingDimensions)
	knowledgeHandler := api.NewKnowledgeHandler(knowledgeService).SetAuditSink(auditStore)

	taskStore := postgres.NewTaskStore(db)
	taskService := task.NewService(taskStore, nil, nil)
	taskHandler := api.NewTaskHandler(taskService).SetAuditSink(auditStore)

	// The browser application (ADR-016) is embedded in the binary. Loading it
	// here turns a missing asset into a startup failure rather than a runtime
	// 404 on a blank page.
	webAssets, err := webui.Load()
	if err != nil {
		logger.NewEntry("webui-load-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	mux := http.NewServeMux()

	// Handlers for exactly the canonical route list in internal/api.Routes.
	// The registration loop below refuses to start if the two disagree, so a
	// route cannot exist in the router without being declared, or be declared
	// without being served.
	handlers := map[api.Route]http.HandlerFunc{
		{Method: http.MethodGet, Pattern: "/health/live"}: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		},
		{Method: http.MethodGet, Pattern: "/health/ready"}: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"ready":true}`))
		},

		// Authentication endpoints. Login/refresh/logout/bootstrap are explicit
		// public exemptions (unauthenticated by design); every other route
		// requires an explicit authorization rule.
		{Method: http.MethodPost, Pattern: "/api/auth/bootstrap"}: authHandler.Bootstrap,
		{Method: http.MethodPost, Pattern: "/api/auth/login"}:     authHandler.Login,
		{Method: http.MethodPost, Pattern: "/api/auth/refresh"}:   authHandler.Refresh,
		{Method: http.MethodPost, Pattern: "/api/auth/logout"}:    authHandler.Logout,
		{Method: http.MethodGet, Pattern: "/api/me"}:              authHandler.Me,

		// Workspace administration. All three routes are protected (no public
		// exemption), so they are reachable only through an explicit allow rule:
		// GET/POST /workspaces are founder organization-level (ScopeNone), and
		// GET /workspaces/{id} is founder org-level or the workspace admin's own
		// workspace (ScopePath). The handlers never accept a client-supplied
		// workspace: the lookup is bound to the verified claims.
		{Method: http.MethodGet, Pattern: "/workspaces"}:      workspaceHandler.List,
		{Method: http.MethodPost, Pattern: "/workspaces"}:     workspaceHandler.Create,
		{Method: http.MethodGet, Pattern: "/workspaces/{id}"}: workspaceHandler.Get,

		// Audit visibility. Read-only: none of these accept a body, and the
		// chain remains append-only behind the audit store.
		{Method: http.MethodGet, Pattern: "/audit/events"}:                 auditHandler.ListOrg,
		{Method: http.MethodGet, Pattern: "/audit/verification"}:           auditHandler.Verification,
		{Method: http.MethodGet, Pattern: "/workspaces/{id}/audit/events"}: auditHandler.ListForWorkspace,

		// Task management. Every route is protected, so each is reachable only
		// through an explicit rbac rule; the workspace each operates on comes
		// from the verified claims and is bound in PostgreSQL by the store.
		{Method: http.MethodPost, Pattern: "/tasks"}:                 taskHandler.Create,
		{Method: http.MethodGet, Pattern: "/tasks"}:                  taskHandler.List,
		{Method: http.MethodGet, Pattern: "/tasks/{id}"}:             taskHandler.Get,
		{Method: http.MethodPatch, Pattern: "/tasks/{id}"}:           taskHandler.Update,
		{Method: http.MethodPost, Pattern: "/tasks/{id}/transition"}: taskHandler.Transition,

		// Knowledge management. Same shape as tasks: every route is protected,
		// so each is reachable only through an explicit RBAC rule, and the store
		// runs on the unprivileged runtime handle so row-level security applies.
		{Method: http.MethodPost, Pattern: "/knowledge"}:        knowledgeHandler.Create,
		{Method: http.MethodGet, Pattern: "/knowledge"}:         knowledgeHandler.List,
		{Method: http.MethodGet, Pattern: "/knowledge/{id}"}:    knowledgeHandler.Get,
		{Method: http.MethodPatch, Pattern: "/knowledge/{id}"}:  knowledgeHandler.Update,
		{Method: http.MethodDelete, Pattern: "/knowledge/{id}"}: knowledgeHandler.Delete,
		{Method: http.MethodPost, Pattern: "/knowledge/search"}: knowledgeHandler.Search,

		// Publishing. Lifecycle changes are named commands, never arbitrary status
		// writes; the handler binds the workspace and actor from verified claims.
		{Method: http.MethodPost, Pattern: "/publications"}:              publicationHandler.Create,
		{Method: http.MethodGet, Pattern: "/publications"}:               publicationHandler.List,
		{Method: http.MethodGet, Pattern: "/publications/{id}"}:          publicationHandler.Get,
		{Method: http.MethodPost, Pattern: "/publications/{id}/submit"}:  publicationHandler.Submit,
		{Method: http.MethodPost, Pattern: "/publications/{id}/approve"}: publicationHandler.Approve,
		{Method: http.MethodPost, Pattern: "/publications/{id}/reject"}:  publicationHandler.Reject,
		{Method: http.MethodPost, Pattern: "/publications/{id}/publish"}: publicationHandler.Publish,
		{Method: http.MethodPost, Pattern: "/publications/{id}/retry"}:   publicationHandler.Retry,

		// Memory is deliberately limited to key-based read/write operations.
		{Method: http.MethodGet, Pattern: "/memory/{layer}/{key}"}: memoryHandler.Read,
		{Method: http.MethodPut, Pattern: "/memory/{layer}/{key}"}: memoryHandler.Write,
	}

	// Registration is a separate function so the wiring can be exercised by a
	// test instead of only by starting the whole server. A mismatch here is a
	// build defect, so main() treats it as fatal.
	if err := registerRoutes(mux, handlers, webAssets); err != nil {
		logger.NewEntry("route-registration-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	// Deny-by-default: every protected route must carry an explicit allow rule.
	// Any request without an explicit permission for its action/resource is DENIED.
	// The RBAC contract is documented in internal/rbac; only the rules for
	// routes actually registered by this server are seeded — a route declared
	// in the contract but not registered stays denied.
	authzService.AddRules(rbac.ImplementedRules())
	protected := authHandler.RequireAuth(authzMiddleware(authzService, mux, auditStore))

	logger.NewEntry("austro-os-serving").With("address", cfg.ServerAddress).Log()

	// Explicit server timeouts. http.ListenAndServe installs none, which leaves
	// the listener open to slow-header and slow-body clients holding a
	// connection indefinitely. The values are set against the actual request
	// shape: bodies are capped at 1 MiB and responses are small JSON documents,
	// so these bounds are generous for any legitimate client while still
	// reclaiming a stalled connection.
	srv := &http.Server{
		Addr:              cfg.ServerAddress,
		Handler:           middleware.Middleware(protected),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown: on SIGINT/SIGTERM stop accepting, let in-flight
	// requests finish within a bounded drain, then return so the deferred
	// database/Redis/broker closes actually run. Without this the process is
	// killed on the signal and those defers never execute.
	idleClosed := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		logger.NewEntry("austro-os-shutting-down").Log()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.NewEntry("austro-os-shutdown-drain-incomplete").SetLevel("error").WithError(err).Log()
		}
		close(idleClosed)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.NewEntry("http-server-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
	<-idleClosed
	logger.NewEntry("austro-os-stopped").Log()
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
	// The embedded browser application serves fixed assets and nothing else: no
	// handler behind these paths reads the body, the query, the path values or a
	// cookie, so they carry no data and cannot widen access. The list is taken
	// from the package that serves them, which keeps the exemption and the
	// registration from drifting apart in either direction.
	if method == http.MethodGet || method == http.MethodHead {
		for _, asset := range webui.Assets() {
			if path == asset.Pattern {
				return true
			}
		}
	}
	return false
}

// authzMiddleware wraps the mux with the deny-by-default authorization
// enforcement point. Rules are explicit; there is no permissive fallback. The
// auth handler's RequireAuth middleware runs first (it activates before this
// layer) so verified claims are present on the request context when an action
// is authorized.
// registerRoutes wires the JSON handlers to the canonical route table in
// internal/api and adds the embedded browser assets.
//
// It refuses to build a router that disagrees with the declared surface in
// either direction. A route declared in api.Routes() with no handler would be a
// documented endpoint that 404s; a handler with no route is dead code that the
// next reader will assume is reachable. Both are build defects, so both are
// errors rather than warnings.
func registerRoutes(mux *http.ServeMux, handlers map[api.Route]http.HandlerFunc, assets map[webui.Asset][]byte) error {
	for _, route := range api.Routes() {
		handler, ok := handlers[route]
		if !ok {
			return fmt.Errorf("route %s is declared in api.Routes() but has no handler", route)
		}
		mux.HandleFunc(route.String(), handler)
		delete(handlers, route)
	}
	for route := range handlers {
		return fmt.Errorf("handler is registered for %s, which is not declared in api.Routes()", route)
	}

	// Static browser application. These serve fixed assets and nothing else: no
	// handler behind them reads the body, the query, the path values or a
	// cookie, which is what makes them safe to exempt from authorization.
	for _, asset := range webui.Assets() {
		body, ok := assets[asset]
		if !ok {
			return fmt.Errorf("asset %s is declared by webui.Assets() but was not loaded", asset.Pattern)
		}
		mux.HandleFunc(asset.Method+" "+asset.Pattern, webui.Handler(asset, body))
	}
	return nil
}

func authzMiddleware(a *authz.Authorizer, next http.Handler, audits audit.Sink) http.Handler {
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
			// A denied authorization attempt is security-relevant evidence:
			// it is what a privilege-escalation probe looks like. The response
			// is already a refusal, so a persistence failure cannot make it
			// look like a success and is logged rather than propagated.
			if audits != nil {
				cv := middleware.ExtractContextValues(r)
				rec := audit.Record{
					EventType:  "authz.denied",
					ActorType:  "user",
					TargetType: "route",
					Outcome:    "denied",
					Principle:  "Security by Design",
					Details:    map[string]any{"action": action, "resource": resource},
				}
				if id, perr := uuid.Parse(cv.TraceID); perr == nil {
					rec.TraceID = id
				}
				if _, aerr := audits.Append(r.Context(), rec); aerr != nil {
					logger.NewEntry("audit-persist-failed").SetLevel("error").
						With("event_type", rec.EventType).WithError(aerr).Log()
				}
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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
