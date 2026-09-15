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
	"austro-os/internal/composition"
	"austro-os/internal/config"
	logger "austro-os/internal/log"
	"austro-os/internal/worker"
)

func main() {
	// Runtime entrypoints use LoadStrict: missing or insecure settings fail fast
	// instead of silently degrading to development defaults. There is no
	// insecure default RabbitMQ fallback here.
	cfg, err := config.LoadStrict()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid configuration: %v\n", err)
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
		With("ai_backend", rt.AIBackend).
		With("publish_backend", rt.PublishBackend).
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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-stop
		logger.NewEntry("worker-shutting-down").Log()
		wp.Stop()
		os.Exit(0)
	}()

	select {}
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
