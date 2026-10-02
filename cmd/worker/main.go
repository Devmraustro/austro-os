package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"austro-os/infrastructure/auditstore"
	"austro-os/infrastructure/database"
	"austro-os/infrastructure/postgres"
	"austro-os/infrastructure/rabbitmq"
	"austro-os/internal/buildinfo"
	"austro-os/internal/composition"
	"austro-os/internal/config"
	logger "austro-os/internal/log"
	"austro-os/internal/worker"
)

func main() {
	// Build identity is answered before configuration, for the same reason as
	// the API: identifying a binary must not require a working environment.
	if len(os.Args) > 1 && buildinfo.IsVersionArg(os.Args[1]) {
		buildinfo.PrintVersion(os.Stdout, "worker")
		return
	}

	// Runtime entrypoints use LoadStrict: missing or insecure settings fail fast
	// instead of silently degrading to development defaults. There is no
	// insecure default RabbitMQ fallback here.
	cfg, err := config.LoadStrict()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid configuration: %v\n", err)
		os.Exit(1)
	}

	// This binary is the worker and nothing else. Asserting the role here rather
	// than overriding it is deliberate: on a platform where one environment file
	// can feed several units, a shared AUSTRO_PROCESS_ROLE is the most likely
	// way this process gets configured wrongly, and silently correcting it would
	// leave the API's role in the file looking intentional while the worker ran
	// under a different one. Refusing names the file to fix.
	if err := assertWorkerRole(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// The worker publishes its cascade events through a self-supervised sink:
	// when the broker force-closes the connection, the sink redials instead of
	// silently dropping pipeline-advance events.
	queueName := queueFor(cfg)
	sink, err := rabbitmq.NewReconnectingSink(cfg.RabbitMQURL, queueName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to RabbitMQ: %v\n", err)
		os.Exit(1)
	}
	defer sink.Close()

	// Same topology as the API: bootstrap as the owner principal, then run on
	// the unprivileged runtime pool that the workspace policies constrain.
	// MustInitializeTopology refuses to return a pool that is a superuser,
	// holds BYPASSRLS, owns a protected table, or can see across workspaces, so
	// the worker cannot come up on a privileged connection.
	handles := database.MustInitializeTopology(cfg)
	db := handles.Runtime
	defer db.Close()
	defer handles.Admin.Close()

	// Liveness evidence for the API process. This worker is the only consumer of
	// the execution queue, so an API in a split deployment has no other way to
	// tell a draining queue from an abandoned one; see startHeartbeat.
	stopHeartbeat := startHeartbeat(cfg, db)
	defer stopHeartbeat()

	// Pipeline and publishing decisions the worker processes are audited to the
	// same persistent chain the API writes, so a restart of either process
	// continues one chain rather than starting a second one.
	auditCtx, auditCancel := context.WithTimeout(context.Background(), 30*time.Second)
	auditStore, err := auditstore.New(auditCtx, handles.Admin)
	auditCancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize audit store: %v\n", err)
		os.Exit(1)
	}

	rt, err := composition.Compose(cfg, composition.Stores{
		Publications: postgres.NewPublicationStore(db),
		Pipelines:    postgres.NewPipelineStore(db),
		Executions:   postgres.NewExecutionStore(db),
	}, composition.Sinks{
		AIDecision:    rabbitmq.NewDecisionSink(sink),
		PublishAudit:  auditstore.NewPublishSink(auditStore),
		PipelineAudit: auditstore.NewPipelineSink(auditStore),
		PublishEvent:  rabbitmq.NewPublishEventSink(sink).Publish,
		PipelineEvent: rabbitmq.NewPipelineEventSink(sink).PublishPipeline,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to compose runtime: %v\n", err)
		os.Exit(1)
	}

	logger.NewEntry("worker-composed").
		With("version", buildinfo.Version).
		With("commit", buildinfo.Commit).
		With("ai_backend", rt.AIBackend).
		With("publish_backend", rt.PublishBackend).
		With("execution_worker", rt.ExecutionWorker != nil).
		With("usage_limit_per_workspace", cfg.AIUsageLimitPerWorkspace).
		Log()

	handler := newHandler(rt)

	wConfig := worker.WorkerConfig{
		URL:       cfg.RabbitMQURL,
		QueueName: queueName,
	}

	wp := worker.NewWorker(wConfig, handler)

	if err := wp.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start worker: %v\n", err)
		os.Exit(1)
	}

	// AI execution dispatch consumer. The composed runtime derives the worker
	// from the same execution store and provider the API admits into; it runs on
	// the dedicated execution queue so dispatch messages never collide with
	// pipeline/publication events. The store being unable to materialize is a
	// composition defect, so the worker process refuses to start without it.
	if rt.ExecutionWorker == nil {
		fmt.Fprintf(os.Stderr, "Composition did not wire the AI execution worker\n")
		os.Exit(1)
	}
	execWP := worker.NewWorker(worker.WorkerConfig{
		URL: cfg.RabbitMQURL,
		// The same AUSTRO_EXECUTION_QUEUE the dispatcher publishes to. Both
		// processes resolve it from configuration (defaulting to
		// config.DefaultExecutionQueue) so a deployment that renames the queue
		// renames it in one place; a name pinned here instead would leave the
		// dispatcher publishing into a queue nothing consumes, with both
		// processes reporting healthy.
		QueueName: cfg.ExecutionQueue,
	}, newExecutionHandler(rt.ExecutionWorker))
	if err := execWP.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start execution worker: %v\n", err)
		os.Exit(1)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, shutdownSignals()...)

	go func() {
		<-stop
		logger.NewEntry("worker-shutting-down").Log()
		// Stop() cancels the consumers and closes the broker connection. A
		// message still in flight at that point is unacknowledged, so the broker
		// requeues it for whoever consumes next rather than dropping it; the
		// execution's lease is left to expire and the dispatcher's stale-lease
		// recovery reschedules it. Both halves of that are why a restart here is
		// safe rather than merely survivable.
		wp.Stop()
		execWP.Stop()
		os.Exit(0)
	}()

	select {}
}

// assertWorkerRole refuses any role this binary cannot serve.
//
// The alternative — overriding the role to "worker" — was rejected because the
// configuration would then describe something different from what is running.
// On a hosting platform where one environment file can feed several units, that
// is the more likely failure mode than a genuinely mislabelled binary, and
// quietly correcting it leaves the file's AUSTRO_PROCESS_ROLE looking deliberate
// while the process does something else. An error naming the setting is the only
// response that sends the reader to the file.
func assertWorkerRole(cfg *config.Config) error {
	if cfg.ProcessRoleName() != config.ProcessRoleWorker {
		return fmt.Errorf(
			"AUSTRO_PROCESS_ROLE is %q but this binary is the worker; set AUSTRO_PROCESS_ROLE=worker in this unit's environment",
			cfg.ProcessRole)
	}
	return nil
}

// shutdownSignals is the set of signals that begin a graceful shutdown.
//
// SIGHUP is included because AlwaysData restarts a background Service by
// sending SIGHUP to its foreground process
// (https://help.alwaysdata.com/en/docs/web-hosting/services). SIGHUP is not
// handled by the Go runtime by default, so without it here a platform restart
// would kill this process outright and the shutdown path above — which closes
// the broker connection rather than dropping it — would never run. SIGINT and
// SIGTERM cover systemd, containers and an operator's kill.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

// queueFor mirrors rabbitmq.Initialize and the API entrypoint: the configured
// queue name, or the default austro.events when unset. The worker must resolve
// the name exactly as the API does so both publish to and consume from the same
// queue even when AUSTRO_RABBITMQ_QUEUE is not set.
func queueFor(cfg *config.Config) string {
	if cfg.RabbitMQQueue != "" {
		return cfg.RabbitMQQueue
	}
	return "austro.events"
}
