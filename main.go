package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"austro-os/infrastructure/auditstore"
	"austro-os/infrastructure/database"
	"austro-os/infrastructure/postgres"
	"austro-os/infrastructure/rabbitmq"
	"austro-os/infrastructure/redis"
	"austro-os/internal/ai"
	"austro-os/internal/aiemployee"
	"austro-os/internal/api"
	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/authz"
	"austro-os/internal/buildinfo"
	"austro-os/internal/composition"
	"austro-os/internal/config"
	"austro-os/internal/department"
	"austro-os/internal/execution"
	"austro-os/internal/health"
	"austro-os/internal/knowledge"
	logger "austro-os/internal/log"
	"austro-os/internal/memory"
	"austro-os/internal/middleware"
	"austro-os/internal/rbac"
	"austro-os/internal/task"
	"austro-os/internal/team"
	"austro-os/internal/webui"

	"github.com/google/uuid"
)

// shutdownDrainTimeout bounds how long graceful shutdown waits for in-flight
// requests. It is comfortably above the 60s write timeout so a request that is
// still progressing is allowed to finish, while guaranteeing the process exits
// rather than hanging on a stalled client.
const shutdownDrainTimeout = 75 * time.Second

func main() {
	// Build identity is answered before anything else, including configuration.
	// A `version` subcommand that required a valid production environment would be
	// useless exactly where it is most needed: on a host where the environment is
	// misconfigured, or on a binary an operator is trying to identify. Nothing
	// here reads the environment, so this cannot leak configuration.
	if len(os.Args) > 1 && buildinfo.IsVersionArg(os.Args[1]) {
		buildinfo.PrintVersion(os.Stdout, "api")
		return
	}

	cfg, err := config.LoadStrict()
	if err != nil {
		logger.NewEntry("invalid-configuration").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	// This binary is either the API or the dispatcher, never the worker. A
	// worker role here would otherwise pass configuration validation and then
	// fall through to the HTTP path, opening a listener for a role that was
	// explicitly declared as holding none — and the declaration would be the
	// only evidence that it went wrong.
	if err := assertServableRole(cfg); err != nil {
		logger.NewEntry("invalid-process-role").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	// Dispatcher-only process. On a hosting model where the HTTP process is not
	// the one with dependable supervision, the sweep loop runs here as its own
	// supervised process and this binary starts no listener at all, so it holds
	// no port and is unreachable from the network. It is the same binary, the
	// same release, and the same code path for building the transport; only the
	// supervision boundary moves.
	//
	// The branch is on the validated role/mode pair, not on the mode alone, so a
	// role that is not the API can never fall through into the HTTP path and
	// open a listener it was configured not to have.
	if isDispatcherRole(cfg) {
		runDispatchService(cfg)
		return
	}

	// Bootstrap the schema as the owner principal, then serve on the
	// unprivileged runtime pool. MustInitializeTopology verifies that pool
	// against the live database before returning, so a process that reaches
	// this line is running as a role the workspace policies actually constrain.
	handles := database.MustInitializeTopology(cfg)
	db := handles.Runtime
	defer db.Close()
	defer handles.Admin.Close()

	// Readiness is built here, over the runtime handle, so the health endpoint
	// can answer with the deployment's real shape: which companions it declared
	// and which of them are actually beating. A single-process deployment has no
	// declared companions, so the endpoint keeps its historical unconditional
	// verdict and nothing changes for it.
	readiness := newReadinessEvaluator(cfg, db)

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

	// AI execution store. It is created before composition because the composed
	// runtime derives the admission service and the worker from it; the same
	// instance is reused below for dispatch, keeping one store per process.
	executionStore := postgres.NewExecutionStore(db)

	// The API composes the full runtime with the production adapters: PostgreSQL
	// stores, Redis memory, RabbitMQ event/audit sinks. This proves the wiring
	// and keeps backend selection honest on a live process.
	rt, err := composition.Compose(cfg, composition.Stores{
		Publications: postgres.NewPublicationStore(db),
		Pipelines:    postgres.NewPipelineStore(db),
		Executions:   executionStore,
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
	pipelineHandler := api.NewPipelineHandler(rt.Orchestration)

	logger.NewEntry("austro-os-startup").
		With("version", buildinfo.Version).
		With("commit", buildinfo.Commit).
		With("environment", cfg.Environment).
		With("composition", "ok").
		With("ai_backend", rt.AIBackend).
		With("publish_backend", rt.PublishBackend).
		With("execution_transport_backend", rt.ExecutionTransportBackend).
		With("usage_limit_per_workspace", cfg.AIUsageLimitPerWorkspace).
		Log()

	// AI execution dispatch. The dispatcher is the only consumer of the claim
	// and stale-reclaim paths (fenced in SQL by lease owner and generation), and
	// one sweep loop per configured workspace runs alongside the API. The loop
	// is deliberately not started when no workspace ids are configured, which is
	// the off switch for deployments that want admission without a local
	// dispatcher, and it is not started at all when AUSTRO_EXECUTION_DISPATCH_MODE
	// moves it to another process.
	var stopDispatch func()
	switch {
	case isDispatcherRole(cfg):
		// Unreachable: the dispatcher-only process returned above. Kept so that
		// the mode check and the mode that is honoured cannot drift apart.
		stopDispatch = func() {}
	case !sweepsHere(cfg):
		// This API process is not the dispatcher. The deployment said who is, so
		// the claim is checked against observed liveness rather than assumed:
		// declaring a companion is not evidence that it is running, and the
		// readiness endpoint reports its heartbeat either way.
		logger.NewEntry("execution-dispatch-idle").
			With("reason", "AUSTRO_EXECUTION_DISPATCH_MODE is "+dispatchMode(cfg)).
			With("declared_companions", cfg.DeclaredCompanions()).
			With("note", "a separate process owns the dispatch sweep").
			Log()
	case len(cfg.ExecutionWorkspaceIDs) == 0:
		logger.NewEntry("execution-dispatch-idle").
			With("reason", "no AUSTRO_EXECUTION_WORKSPACE_IDS configured").Log()
	default:
		stopDispatch, err = startDispatchSweep(cfg, executionStore)
		if err != nil {
			logger.NewEntry("execution-transport-sink-failed").SetLevel("error").WithError(err).Log()
			os.Exit(1)
		}
	}
	if stopDispatch != nil {
		defer stopDispatch()
	}

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

	// Organization hierarchy: Departments → Teams → AI Employees.
	// Stores run on runtime handle so RLS confines them; workspace is bound
	// from verified claims in each store's transaction.
	departmentStore := postgres.NewDepartmentStore(db)
	departmentService := department.NewService(departmentStore)
	departmentHandler := api.NewDepartmentHandler(departmentService).SetAuditSink(auditStore)

	teamStore := postgres.NewTeamStore(db)
	teamResolver := postgres.NewDepartmentResolverAdapter(departmentStore)
	teamService := team.NewService(teamStore, teamResolver)
	teamHandler := api.NewTeamHandler(teamService).SetAuditSink(auditStore)

	aiEmployeeStore := postgres.NewAIEmployeeStore(db)
	teamResolverForEmployee := postgres.NewTeamResolverAdapter(teamStore)
	aiEmployeeService := aiemployee.NewService(aiEmployeeStore, teamResolverForEmployee)
	aiEmployeeHandler := api.NewAIEmployeeHandler(aiEmployeeService).SetAuditSink(auditStore)

	// AI execution. The store runs on the unprivileged runtime handle so RLS
	// confines executions to the caller's workspace. The handler composes the
	// task and employee services so the admission snapshot is built only from
	// data the caller can already see; admission captures that snapshot in the
	// same transaction that persists the execution. The service itself comes
	// from the composed runtime (rt.Execution), built from the same store.
	executionHandler := api.NewExecutionHandler(rt.Execution, taskService, aiEmployeeService).SetAuditSink(auditStore)

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
			if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
				return
			}
		},
		{Method: http.MethodGet, Pattern: "/health/ready"}: func(w http.ResponseWriter, r *http.Request) {
			report, err := readiness.Evaluate(r.Context())
			if err != nil {
				// The check could not be completed, so the answer is unknown.
				// Reporting ready here would be the exact defect this endpoint
				// was changed to remove: a process that cannot see its
				// dependencies is not thereby known to have them.
				logger.NewEntry("readiness-unavailable").
					SetLevel("warn").
					WithError(err).
					Log()
			}
			// The evaluator fills in the same report on failure, including the
			// reason and whatever it did manage to observe, so the response is
			// rendered through one path. A 503 body that omitted status and
			// requires would contradict the documented schema for exactly the
			// response an operator most needs to trust.
			writeReadiness(w, report)
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

		// AI execution. Admission, the single read and human cancellation are
		// protected routes reached only through an explicit rbac rule; the
		// workspace comes from the verified claims and RLS binds it in the store.
		{Method: http.MethodPost, Pattern: "/tasks/{id}/execute"}:     executionHandler.Execute,
		{Method: http.MethodGet, Pattern: "/executions/{id}"}:         executionHandler.Get,
		{Method: http.MethodPost, Pattern: "/executions/{id}/cancel"}: executionHandler.Cancel,

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

		// Creator pipelines. The worker remains the only advancement engine;
		// HTTP exposes observation, the named human approval handoff and retry.
		{Method: http.MethodPost, Pattern: "/pipelines"}:              pipelineHandler.Create,
		{Method: http.MethodGet, Pattern: "/pipelines"}:               pipelineHandler.List,
		{Method: http.MethodGet, Pattern: "/pipelines/{id}"}:          pipelineHandler.Get,
		{Method: http.MethodPost, Pattern: "/pipelines/{id}/approve"}: pipelineHandler.Approve,
		{Method: http.MethodPost, Pattern: "/pipelines/{id}/retry"}:   pipelineHandler.Retry,

		// Organization hierarchy: Workspace → Departments → Teams → AI Employees.
		{Method: http.MethodPost, Pattern: "/departments"}:        departmentHandler.Create,
		{Method: http.MethodGet, Pattern: "/departments"}:         departmentHandler.List,
		{Method: http.MethodGet, Pattern: "/departments/{id}"}:    departmentHandler.Get,
		{Method: http.MethodPatch, Pattern: "/departments/{id}"}:  departmentHandler.Update,
		{Method: http.MethodDelete, Pattern: "/departments/{id}"}: departmentHandler.Delete,

		{Method: http.MethodPost, Pattern: "/teams"}:        teamHandler.Create,
		{Method: http.MethodGet, Pattern: "/teams"}:         teamHandler.List,
		{Method: http.MethodGet, Pattern: "/teams/{id}"}:    teamHandler.Get,
		{Method: http.MethodPatch, Pattern: "/teams/{id}"}:  teamHandler.Update,
		{Method: http.MethodDelete, Pattern: "/teams/{id}"}: teamHandler.Delete,

		{Method: http.MethodPost, Pattern: "/ai-employees"}:        aiEmployeeHandler.Create,
		{Method: http.MethodGet, Pattern: "/ai-employees"}:         aiEmployeeHandler.List,
		{Method: http.MethodGet, Pattern: "/ai-employees/{id}"}:    aiEmployeeHandler.Get,
		{Method: http.MethodPatch, Pattern: "/ai-employees/{id}"}:  aiEmployeeHandler.Update,
		{Method: http.MethodDelete, Pattern: "/ai-employees/{id}"}: aiEmployeeHandler.Delete,

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
		signal.Notify(sig, shutdownSignals()...)
		<-sig
		logger.NewEntry("austro-os-shutting-down").Log()
		ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.NewEntry("austro-os-shutdown-drain-incomplete").SetLevel("error").WithError(err).Log()
		}
		if stopDispatch != nil {
			stopDispatch()
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

// shutdownSignals is the set of signals that begin a graceful shutdown.
//
// SIGHUP is here deliberately, and it is not a Unix-only courtesy. AlwaysData
// restarts a background Service by sending SIGHUP to its foreground process
// (https://help.alwaysdata.com/en/docs/web-hosting/services). SIGHUP is not in
// Go's default-handled set, so without this line the runtime's default action
// applies and the process dies where it stands: no request drain, no deferred
// connection close, no log line explaining why. That is survivable for this
// system — the dispatch and consumption protocols are fenced and at-least-once,
// so a hard kill loses only work that the next sweep reclaims — but paying for
// it on every platform restart is a choice, not a necessity.
// SIGINT and SIGTERM cover systemd, containers and an operator's kill.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

// dispatchMode is the configured sweep-loop owner, normalised the same way the
// configuration validator normalises it.
func dispatchMode(cfg *config.Config) string {
	return strings.ToLower(strings.TrimSpace(cfg.ExecutionDispatchMode))
}

// sweepsHere reports whether this process starts the dispatch sweep loop.
func sweepsHere(cfg *config.Config) bool {
	return cfg.DispatchesHere()
}

// isDispatcherRole reports whether this process is the dedicated dispatcher:
// sweep loop, no HTTP listener, no inbound port. The entrypoint branches on the
// role rather than on the mode, because the role is the statement about what
// this process is and the mode is only one of its properties.
func isDispatcherRole(cfg *config.Config) bool {
	return cfg.ProcessRoleName() == config.ProcessRoleDispatcher
}

// assertServableRole refuses a role this binary cannot serve.
//
// The worker is the only rejection, and it is a real one: role=worker with
// mode=off is a valid configuration, so Validate accepts it, and without this
// check the API binary would fall through the dispatcher branch and open an HTTP
// listener for a process that was explicitly declared to hold none. The declared
// role would be the only evidence that anything went wrong, and it would be
// evidence nobody reads.
func assertServableRole(cfg *config.Config) error {
	if cfg.ProcessRoleName() == config.ProcessRoleWorker {
		return errors.New("AUSTRO_PROCESS_ROLE is " + config.ProcessRoleWorker +
			" but this binary serves the api and dispatcher roles; the worker is cmd/worker")
	}
	return nil
}

// newExecutionPublisher builds the dispatch transport for whichever process is
// sweeping, and returns the cleanup for it. Both processes call this, so the
// API and the standalone dispatcher cannot drift onto different transports or
// different queue names — a drift there is invisible from inside either
// process, because the failure lands on the other side of the broker.
func newExecutionPublisher(cfg *config.Config) (execution.Publisher, func(), error) {
	switch cfg.ExecutionTransportBackend {
	case "", config.ExecutionTransportBackendStub:
		return execution.NewStubPublisher(), func() {}, nil
	case config.ExecutionTransportBackendRabbitMQ:
		// The queue name comes from configuration (AUSTRO_EXECUTION_QUEUE,
		// default config.DefaultExecutionQueue) for the same reason the worker
		// resolves it from configuration: the publisher and the consumer are
		// separate processes and a name pinned on only one of them silently
		// splits the pipeline in half.
		execSink, err := rabbitmq.NewReconnectingSink(cfg.RabbitMQURL, cfg.ExecutionQueue)
		if err != nil {
			return nil, nil, err
		}
		return rabbitmq.NewExecutionPublisher(execSink), func() { execSink.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unknown execution transport backend %q", cfg.ExecutionTransportBackend)
	}
}

// startDispatchSweep runs one sweep goroutine per configured workspace and
// returns a stop function that cancels them and closes the transport. It is
// shared by the API and the standalone dispatcher for the same reason the
// publisher factory is: the dispatch topology must be one definition, not two
// that agree today.
func startDispatchSweep(cfg *config.Config, store execution.ExecutionStore) (func(), error) {
	pub, cleanup, err := newExecutionPublisher(cfg)
	if err != nil {
		return nil, err
	}
	dispatcher := execution.NewDispatcher(
		store, pub, execution.NewWorkerID(),
		cfg.ExecutionLeaseTTL, cfg.ExecutionRetryBackoffBase, cfg.ExecutionRetryBackoffMax,
	)
	ctx, cancel := context.WithCancel(context.Background())
	for _, ws := range cfg.ExecutionWorkspaceIDs {
		go dispatcher.Run(ctx, ws, cfg.ExecutionSweepInterval)
	}
	logger.NewEntry("execution-dispatch-started").
		With("workspaces", len(cfg.ExecutionWorkspaceIDs)).
		With("transport", cfg.ExecutionTransportBackend).
		With("queue", cfg.ExecutionQueue).
		With("owner", dispatchMode(cfg)).
		Log()
	return func() {
		cancel()
		cleanup()
	}, nil
}

// runDispatchService is the dedicated dispatcher process: sweep loop only.
//
// It opens no listening socket, so it needs no port in the platform's reserved
// range and is not reachable from the network. It also composes no HTTP
// handlers and opens no Redis connection, so the credentials it requires are
// the database and the broker and nothing else — the dispatcher has no reason
// to hold a signing key, and not asking for one keeps the blast radius of this
// process equal to the execution queue.
func runDispatchService(cfg *config.Config) {
	handles := database.MustInitializeTopology(cfg)
	defer handles.Runtime.Close()
	defer handles.Admin.Close()

	// The dispatcher is the process an API configured with
	// AUSTRO_PROCESS_ROLE=api and AUSTRO_EXECUTION_DISPATCH_MODE=off is relying
	// on. Registering here is the only way that API can find out whether this
	// process is alive: it cannot see the Service, and this cannot see the API,
	// so the shared table is the whole of the mutual awareness.
	stopHeartbeat := startHeartbeat(cfg, handles.Runtime, config.ProcessRoleDispatcher)
	defer stopHeartbeat()

	if len(cfg.ExecutionWorkspaceIDs) == 0 {
		logger.NewEntry("execution-dispatch-idle").
			With("reason", "no AUSTRO_EXECUTION_WORKSPACE_IDS configured").
			With("note", "this process has no other work to do").
			Log()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, shutdownSignals()...)
		<-sig
		return
	}

	stop, err := startDispatchSweep(cfg, postgres.NewExecutionStore(handles.Runtime))
	if err != nil {
		logger.NewEntry("execution-transport-sink-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
	defer stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, shutdownSignals()...)
	<-sig
	logger.NewEntry("dispatch-service-stopped").Log()
}

// writeReadiness renders a health.Report as the readiness response.
//
// The "ready" field stays the first key and keeps its old boolean shape, so the
// existing deployment scripts that match on the body keep working. Everything
// after it is new: the status code and the declared/observed companion state
// are what make the split deployment checkable, and a 503 is what makes a
// missing dispatcher visible to a platform supervisor that only ever looks at
// the code.
func writeReadiness(w http.ResponseWriter, report health.Report) {
	type companionView struct {
		Role      string `json:"role"`
		Instances int    `json:"instances"`
		LastSeen  string `json:"last_seen,omitempty"`
	}
	companions := make([]companionView, 0, len(report.Companions))
	for _, c := range report.Companions {
		view := companionView{Role: c.Role, Instances: c.Instances}
		if !c.LastSeen.IsZero() {
			// RFC 3339 in UTC: the timestamp is compared against a
			// database-side clock, so a reader in another timezone must not be
			// left interpreting it.
			view.LastSeen = c.LastSeen.UTC().Format(time.RFC3339)
		}
		companions = append(companions, view)
	}

	body := struct {
		Ready  bool   `json:"ready"`
		Status string `json:"status"`
		Reason string `json:"reason,omitempty"`
		// Required is never omitted. An empty list is a meaningful answer —
		// "this deployment needs no companion" — and dropping the key would
		// leave a reader unable to tell that apart from a response that never
		// evaluated the question.
		Required []string        `json:"requires"`
		Roles    []companionView `json:"companions,omitempty"`
	}{
		Ready:    report.Ready,
		Status:   readinessStatus(report.Ready),
		Reason:   report.Reason,
		Required: report.Required,
		Roles:    companions,
	}
	if body.Required == nil {
		body.Required = []string{}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		// The body is built from strings, ints and a bool, so this cannot
		// happen; reporting not-ready is the only honest response if it does.
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !report.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	if _, err := w.Write(payload); err != nil {
		return
	}
}

func readinessStatus(ready bool) string {
	if ready {
		return "ok"
	}
	return "degraded"
}

// newReadinessEvaluator builds the readiness check for this process.
//
// Dependencies come from the roles this deployment declared, not from a fixed
// list. Two reasons, and the second is the one that bites:
//
// First, a process must not be able to configure its way out of checking for
// the companions it declared, so the declared set drives the check.
//
// Second, an undeclared role must not be queried at all. Listing both roles
// unconditionally would make a single-process deployment — which declares
// nothing — depend on a database round trip it has no reason to make. If that
// query fails, the endpoint answers 503 for a deployment that has no companion
// to be missing, which is a worse failure than the stub it replaced: a host
// that loses its database for an unrelated reason would take every single-process
// site out of rotation, and the only evidence would be a warning nobody reads.
//
// Every declared companion is required. There is no "declared but optional" state:
// naming a role in AUSTRO_EXPECT_COMPANION_PROCESSES is a claim that this process
// cannot serve traffic without it, and the operator who believes otherwise has
// not declared it yet.
func newReadinessEvaluator(cfg *config.Config, db *sql.DB) *health.Evaluator {
	return health.NewEvaluator(heartbeatReader{db: db}, readinessDeps(cfg), cfg.HeartbeatStaleAfter)
}

// readinessDeps turns the deployment's declaration into the dependencies the
// evaluator will check.
//
// It is a separate function because the decision is worth testing on its own:
// the entrypoint needs a *sql.DB to build an evaluator, but whether a
// configuration produces zero queries or two is a property of the
// configuration, and testing it through a live database would make the one
// property that must never regress the hardest one to observe.
//
// Every declared companion is required. There is no "declared but optional"
// state: naming a role in AUSTRO_EXPECT_COMPANION_PROCESSES is a claim that
// this process cannot serve traffic without it, and the operator who believes
// otherwise has not declared it yet.
func readinessDeps(cfg *config.Config) []health.Dependency {
	declared := cfg.DeclaredCompanions()
	deps := make([]health.Dependency, 0, len(declared))
	for _, role := range declared {
		deps = append(deps, health.Dependency{Role: role, Required: true})
	}
	return deps
}

// heartbeatReader adapts the PostgreSQL store to the health package's interface.
// It exists so main.go does not depend on the store's concrete type, and so the
// readiness logic can be exercised against a scripted reader instead of a live
// database.
type heartbeatReader struct {
	db *sql.DB
}

func (h heartbeatReader) Liveness(ctx context.Context, role string, staleAfter time.Duration) (health.RoleStatus, error) {
	status, err := postgres.NewHeartbeatStore(h.db).Liveness(ctx, role, staleAfter)
	if err != nil {
		return health.RoleStatus{Role: role}, err
	}
	return health.RoleStatus{
		Role:      status.Role,
		Instances: status.FreshInstances,
		LastSeen:  status.LastSeen,
	}, nil
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
