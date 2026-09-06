package rabbitmq

import (
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	// redialBase is the initial delay before the first automatic reconnection
	// attempt; each failed attempt doubles the delay up to redialMax.
	redialBase = 500 * time.Millisecond
	redialMax  = 5 * time.Second
)

// reconnector owns a supervised AMQP connection that is automatically redialed
// when the broker closes it (e.g. a heartbeat timeout), and vends a fresh
// publish channel per call. AMQP channels are single-goroutine by contract, so
// a per-call channel is the concurrency-safe way to publish from the API's
// concurrent handlers and the worker's message loop. The queue is redeclared on
// every (re)connect (durable/idempotent), so the sink stays hot across outages.
type reconnector struct {
	url     string
	queue   string
	mu      sync.RWMutex
	conn    *amqp.Connection
	stop    chan struct{}
	stopped bool
}

// newReconnector dials once and starts supervision. A failed initial dial fails
// fast (the caller surfaces it); every later disconnect is recovered in-band.
func newReconnector(url, queue string) (*reconnector, error) {
	if queue == "" {
		queue = "austro.events"
	}
	c, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: initial dial failed: %w", err)
	}
	r := &reconnector{
		url:   url,
		queue: queue,
		conn:  c,
		stop:  make(chan struct{}),
	}
	declareQueue(c, queue)
	go r.supervise()
	return r, nil
}

// declareQueue durably declares the events queue on the given connection and
// closes the throwaway channel. Failures are ignored: a later deliver can still
// publish after QueueDeclare settles.
func declareQueue(c *amqp.Connection, queue string) {
	ch, err := c.Channel()
	if err != nil {
		return
	}
	defer ch.Close()
	_, _ = ch.QueueDeclare(queue, true, false, false, false, nil)
}

// supervise reacts to broker-forced closes by redialing with bounded exponential
// backoff until the connection is back or the reconnector is stopped.
func (r *reconnector) supervise() {
	delay := redialBase
	for {
		c := r.current()
		if c == nil {
			return
		}
		errc := c.NotifyClose(make(chan *amqp.Error, 1))
		select {
		case <-errc:
		case <-r.stop:
			return
		}
		for {
			select {
			case <-r.stop:
				return
			default:
			}
			nc, err := amqp.Dial(r.url)
			if err == nil {
				delay = redialBase
				declareQueue(nc, r.queue)
				r.setConn(nc)
				break
			}
			select {
			case <-r.stop:
				return
			case <-time.After(delay):
			}
			delay *= 2
			if delay > redialMax {
				delay = redialMax
			}
		}
	}
}

// channel returns a fresh publish channel from the current live connection.
func (r *reconnector) channel() (*amqp.Channel, error) {
	c := r.current()
	if c == nil {
		return nil, fmt.Errorf("rabbitmq: reconnecting sink is closed")
	}
	ch, err := c.Channel()
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: open publish channel: %w", err)
	}
	return ch, nil
}

// breakConnection force-closes the current connection so supervision redials it.
// It exists as an operational/test hook to prove the sink recovers from a
// broker-side connection loss.
func (r *reconnector) breakConnection() error {
	r.mu.RLock()
	c := r.conn
	r.mu.RUnlock()
	if c == nil {
		return fmt.Errorf("rabbitmq: reconnecting sink has no active connection")
	}
	return c.Close()
}

// close stops supervision and releases the connection.
func (r *reconnector) close() error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	c := r.conn
	r.conn = nil
	close(r.stop)
	r.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

func (r *reconnector) current() *amqp.Connection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conn
}

func (r *reconnector) setConn(c *amqp.Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conn = c
}
