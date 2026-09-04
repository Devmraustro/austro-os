package worker

import (
	"austro-os/internal/event"
	logger "austro-os/internal/log"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

type WorkerConfig struct {
	URL       string
	QueueName string
}

type Worker struct {
	config     WorkerConfig
	connection *amqp.Connection
	channel    *amqp.Channel
	handler    func(*event.UniversalEnvelope) error
	stopCh     chan struct{}
}

func NewWorker(config WorkerConfig, handler func(*event.UniversalEnvelope) error) *Worker {
	return &Worker{
		config:  config,
		handler: handler,
		stopCh:  make(chan struct{}),
	}
}

func (w *Worker) Start() error {
	conn, err := amqp.Dial(w.config.URL)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}
	w.connection = conn

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to open channel: %w", err)
	}
	w.channel = ch

	_, err = ch.QueueDeclare(w.config.QueueName, true, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("failed to declare queue: %w", err)
	}

	err = ch.Qos(1, 0, false)
	if err != nil {
		return fmt.Errorf("failed to set QoS: %w", err)
	}

	msgs, err := ch.Consume(w.config.QueueName, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("failed to register consumer: %w", err)
	}

	go func() {
		for {
			select {
			case <-w.stopCh:
				return
			case msg, ok := <-msgs:
				if !ok {
					return
				}
				go w.processMessage(msg)
			}
		}
	}()

	logger.NewEntry("worker-started").With("queue", w.config.QueueName).Log()
	return nil
}

func (w *Worker) Stop() {
	close(w.stopCh)
	if w.channel != nil {
		w.channel.Close()
	}
	if w.connection != nil {
		w.connection.Close()
	}
	logger.NewEntry("worker-stopped").Log()
}

func (w *Worker) processMessage(msg amqp.Delivery) {
	start := time.Now()
	traceID := msg.Headers["trace_id"]
	spanID := msg.Headers["span_id"]

	logger.NewEntry("worker-message-received").
		With("message_id", msg.MessageId).
		With("trace_id", traceID).
		With("span_id", spanID).
		Log()

	var env event.UniversalEnvelope
	if err := json.Unmarshal(msg.Body, &env); err != nil {
		logger.NewEntry("worker-json-decode-error").
			With("message_id", msg.MessageId).
			With("error", err.Error()).
			Log()
		msg.Ack(false)
		return
	}

	if traceID != nil {
		if tid, ok := traceID.(string); ok {
			parsed, err := uuid.Parse(tid)
			if err != nil {
				logger.NewEntry("worker-invalid-trace-id").
					With("message_id", msg.MessageId).
					WithError(err).
					Log()
			} else {
				env.TraceID = parsed
			}
		}
	}
	if spanID != nil {
		if sid, ok := spanID.(string); ok {
			parsed, err := uuid.Parse(sid)
			if err != nil {
				logger.NewEntry("worker-invalid-span-id").
					With("message_id", msg.MessageId).
					WithError(err).
					Log()
			} else {
				env.SpanID = parsed
			}
		}
	}

	if w.handler != nil {
		if err := w.handler(&env); err != nil {
			logger.NewEntry("worker-handler-error").
				With("message_id", msg.MessageId).
				With("event_type", string(env.EventType)).
				With("error", err.Error()).
				Log()
			msg.Nack(false, true)
			return
		}
	}

	msg.Ack(false)

	duration := time.Since(start)
	logger.NewEntry("worker-message-processed").
		With("message_id", msg.MessageId).
		With("event_type", string(env.EventType)).
		With("duration_ms", duration.Milliseconds()).
		Log()
}

func WithTraceID(traceID string, workerID string) func(*event.UniversalEnvelope) error {
	return func(ev *event.UniversalEnvelope) error {
		ev.TraceID = uuid.MustParse(traceID)
		ev.SpanID = uuid.New()
		return nil
	}
}

func WithCorrelationID(correlationID string) func(*event.UniversalEnvelope) error {
	return func(ev *event.UniversalEnvelope) error {
		ev.Details = json.RawMessage(`{"correlation_id": "` + correlationID + `"}`)
		return nil
	}
}
