package worker

import (
	"austro-os/internal/event"
	logger "austro-os/internal/log"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

type WorkerConfig struct {
	URL       string
	QueueName string
}

// ErrPermanent marks a handler failure that can never succeed on retry: a
// message that is structurally invalid rather than one that hit a transient
// dependency failure.
//
// Without this distinction every handler error is requeued, and a message that
// can never be processed is redelivered forever. Because the consumer runs with
// a prefetch of 1, that single message also blocks every message behind it, so
// one malformed event wedges the whole queue.
//
// A permanent failure is still a failure: it is logged at error level with its
// own event name and then acknowledged only to take it off the queue. It is
// never reported as successfully processed.
var ErrPermanent = errors.New("permanent message failure")

// PermanentErrorf wraps a formatted error as permanent.
func PermanentErrorf(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrPermanent, fmt.Sprintf(format, args...))
}

type WorkerOption func(*Worker)

// WithReconnectDelay overrides the delay before a reconnection attempt after a
// lost connection (default 5s). Used by tests and tunable by operators.
func WithReconnectDelay(d time.Duration) WorkerOption {
	return func(w *Worker) { w.reconnectDelay = d }
}

const maxReconnectDelay = 30 * time.Second

type Worker struct {
	config         WorkerConfig
	handler        func(*event.UniversalEnvelope) error
	reconnectDelay time.Duration

	mu          sync.Mutex
	connection  *amqp.Connection
	channel     *amqp.Channel
	consumeDone <-chan struct{}

	stopOnce sync.Once
	stopCh   chan struct{}
	stopped  chan struct{}
}

func NewWorker(config WorkerConfig, handler func(*event.UniversalEnvelope) error, opts ...WorkerOption) *Worker {
	w := &Worker{
		config:         config,
		handler:        handler,
		reconnectDelay: 5 * time.Second,
		stopCh:         make(chan struct{}),
		stopped:        make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Start connects to the broker and begins consuming. It fails fast only if the
// initial connection is impossible; after the consumer is live, the supervisor
// goroutine reconnects automatically if the connection is ever lost.
func (w *Worker) Start() error {
	if err := w.dial(); err != nil {
		return err
	}
	logger.NewEntry("worker-started").With("queue", w.config.QueueName).Log()
	go w.supervise()
	return nil
}

func (w *Worker) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
	w.dropConnection()
	select {
	case <-w.stopped:
	case <-time.After(2 * time.Second):
	}
	logger.NewEntry("worker-stopped").Log()
}

// CloseBrokerConnection force-closes the current broker connection so the
// supervisor immediately reconnects and re-registers the consumer. Exposed for
// operational reconnection and for tests that exercise the recovery path.
func logCloseError(component string, err error) {
	logger.NewEntry("worker-close-error").
		SetLevel("error").
		With("component", component).
		WithError(err).
		Log()
}

func (w *Worker) CloseBrokerConnection() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.connection != nil {
		if err := w.connection.Close(); err != nil {
			logCloseError("connection", err)
		}
	}
}

func (w *Worker) dial() error {
	conn, err := amqp.Dial(w.config.URL)
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			logCloseError("connection-after-channel-failure", closeErr)
		}
		return fmt.Errorf("failed to open channel: %w", err)
	}

	if _, err := ch.QueueDeclare(w.config.QueueName, true, false, false, false, nil); err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			logCloseError("connection-after-queue-failure", closeErr)
		}
		return fmt.Errorf("failed to declare queue: %w", err)
	}

	if err := ch.Qos(1, 0, false); err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			logCloseError("connection-after-qos-failure", closeErr)
		}
		return fmt.Errorf("failed to set QoS: %w", err)
	}

	msgs, err := ch.Consume(w.config.QueueName, "", false, false, false, false, nil)
	if err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			logCloseError("connection-after-consumer-failure", closeErr)
		}
		return fmt.Errorf("failed to register consumer: %w", err)
	}

	done := w.spawnConsumer(msgs)

	w.mu.Lock()
	w.connection = conn
	w.channel = ch
	w.consumeDone = done
	w.mu.Unlock()

	return nil
}

func (w *Worker) spawnConsumer(msgs <-chan amqp.Delivery) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	return done
}

// supervise keeps the consumer running across broker reconnects: it waits for
// the connection or the deliveries channel to close, then reconnects with
// bounded exponential backoff until it either succeeds or the worker stops.
func (w *Worker) supervise() {
	defer close(w.stopped)
	for {
		w.mu.Lock()
		conn := w.connection
		w.mu.Unlock()
		if conn == nil {
			return
		}

		connClosed := make(chan *amqp.Error, 1)
		conn.NotifyClose(connClosed)

		w.mu.Lock()
		consumeDone := w.consumeDone
		w.mu.Unlock()

		select {
		case <-w.stopCh:
			return
		case <-connClosed:
		case <-consumeDone:
		}

		w.dropConnection()
		if !w.reconnectLoop() {
			return
		}
	}
}

func (w *Worker) reconnectLoop() bool {
	delay := w.reconnectDelay
	for {
		select {
		case <-w.stopCh:
			return false
		case <-time.After(delay):
		}
		if err := w.dial(); err == nil {
			logger.NewEntry("worker-reconnected").With("queue", w.config.QueueName).Log()
			return true
		} else {
			logger.NewEntry("worker-reconnect-failed").With("queue", w.config.QueueName).WithError(err).Log()
		}
		delay *= 2
		if delay > maxReconnectDelay {
			delay = maxReconnectDelay
		}
	}
}

func (w *Worker) dropConnection() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.channel != nil {
		if err := w.channel.Close(); err != nil {
			logCloseError("channel", err)
		}
	}
	if w.connection != nil {
		if err := w.connection.Close(); err != nil {
			logCloseError("connection", err)
		}
	}
	w.channel = nil
	w.connection = nil
	w.consumeDone = nil
}

func logSettlementError(action string, msg amqp.Delivery, err error) {
	logger.NewEntry("worker-message-settlement-error").
		SetLevel("error").
		With("action", action).
		With("message_id", msg.MessageId).
		WithError(err).
		Log()
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
			SetLevel("error").
			With("message_id", msg.MessageId).
			With("error", err.Error()).
			Log()
		if err := msg.Ack(false); err != nil {
			logSettlementError("ack", msg, err)
		}
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
			if errors.Is(err, ErrPermanent) {
				// This message can never be processed. Requeueing it would
				// redeliver it forever and, with a prefetch of 1, block every
				// message behind it. It is logged at error level under its own
				// event name and acknowledged only to take it off the queue;
				// it is never reported as successfully processed.
				logger.NewEntry("worker-message-rejected-permanent").
					SetLevel("error").
					With("message_id", msg.MessageId).
					With("event_type", string(env.EventType)).
					With("error", err.Error()).
					Log()
				if err := msg.Ack(false); err != nil {
					logSettlementError("ack", msg, err)
				}
				return
			}
			// Transient failure: leave it for redelivery.
			logger.NewEntry("worker-handler-error").
				SetLevel("error").
				With("message_id", msg.MessageId).
				With("event_type", string(env.EventType)).
				With("error", err.Error()).
				Log()
			if err := msg.Nack(false, true); err != nil {
				logSettlementError("nack", msg, err)
			}
			return
		}
	}

	if err := msg.Ack(false); err != nil {
		logSettlementError("ack", msg, err)
		return
	}

	duration := time.Since(start)
	logger.NewEntry("worker-message-processed").
		With("message_id", msg.MessageId).
		With("event_type", string(env.EventType)).
		With("duration_ms", duration.Milliseconds()).
		Log()
}

// WithTraceID stamps a fixed trace id onto the envelope and starts a fresh
// span. A trace id that is not a UUID is reported as an error rather than
// panicking: uuid.MustParse would abort the process on malformed input.
func WithTraceID(traceID string, workerID string) func(*event.UniversalEnvelope) error {
	return func(ev *event.UniversalEnvelope) error {
		parsed, err := uuid.Parse(traceID)
		if err != nil {
			return fmt.Errorf("invalid trace id %q: %w", traceID, err)
		}
		ev.TraceID = parsed
		ev.SpanID = uuid.New()
		return nil
	}
}

// WithCorrelationID attaches a correlation id to the envelope details. The
// payload is marshalled rather than concatenated, so a correlation id
// containing a quote or brace cannot break out of the JSON document.
func WithCorrelationID(correlationID string) func(*event.UniversalEnvelope) error {
	return func(ev *event.UniversalEnvelope) error {
		b, err := json.Marshal(map[string]string{"correlation_id": correlationID})
		if err != nil {
			return fmt.Errorf("encode correlation id: %w", err)
		}
		ev.Details = b
		return nil
	}
}
