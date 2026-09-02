package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"austro-os/internal/event"
	logger "austro-os/internal/log"
	"austro-os/internal/worker"
)

func main() {
	rabbitMQURL := os.Getenv("AUSTRO_RABBITMQ_URL")
	if rabbitMQURL == "" {
		rabbitMQURL = "amqp://austro:austro@rabbitmq:5672"
	}

	queueName := os.Getenv("AUSTRO_RABBITMQ_QUEUE")
	if queueName == "" {
		queueName = "austro.events"
	}

	handler := func(env *event.UniversalEnvelope) error {
		logger.NewEntry("worker-handled").
			With("event_id", env.EventID).
			With("event_type", string(env.EventType)).
			With("trace_id", env.TraceID).
			Log()
		return nil
	}

	wConfig := worker.WorkerConfig{
		URL:       rabbitMQURL,
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
