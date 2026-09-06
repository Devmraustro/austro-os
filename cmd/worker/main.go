package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

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
	sink, err := rabbitmq.NewReconnectingSink(cfg.RabbitMQURL, cfg.RabbitMQQueue)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to RabbitMQ: %v\n", err)
		os.Exit(1)
	}
	defer sink.Close()

	db := database.Initialize(cfg)
	defer db.Close()

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
		QueueName: cfg.RabbitMQQueue,
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
