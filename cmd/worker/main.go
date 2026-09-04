package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"austro-os/internal/config"
	"austro-os/internal/event"
	logger "austro-os/internal/log"
	"austro-os/internal/worker"
)

func main() {
	// Runtime entrypoints use LoadStrict: missing or insecure settings fail fast
	// instead of silently degrading to development defaults (fail-fast
	// discipline). There is no insecure default RabbitMQ fallback here.
	cfg, err := config.LoadStrict()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid configuration: %v\n", err)
		os.Exit(1)
	}
	if cfg.RabbitMQURL == "" || cfg.RabbitMQQueue == "" {
		fmt.Fprintf(os.Stderr, "Invalid configuration: required settings missing or insecure: AUSTRO_RABBITMQ_URL, AUSTRO_RABBITMQ_QUEUE\n")
		os.Exit(1)
	}

	queueName := cfg.RabbitMQQueue

	handler := func(env *event.UniversalEnvelope) error {
		logger.NewEntry("worker-handled").
			With("event_id", env.EventID).
			With("event_type", string(env.EventType)).
			With("trace_id", env.TraceID).
			Log()
		return nil
	}

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
