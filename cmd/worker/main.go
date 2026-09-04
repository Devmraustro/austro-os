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

	amqp "github.com/rabbitmq/amqp091-go"
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

	// The worker owns its RabbitMQ channel and queue so the configured queue
	// name is honoured end-to-end.
	conn, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to RabbitMQ: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open RabbitMQ channel: %v\n", err)
		os.Exit(1)
	}
	if _, err := ch.QueueDeclare(cfg.RabbitMQQueue, true, false, false, false, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to declare queue %q: %v\n", cfg.RabbitMQQueue, err)
		os.Exit(1)
	}

	db := database.Initialize(cfg)
	defer db.Close()

	sink := rabbitmq.NewSink(ch, cfg.RabbitMQQueue)

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
