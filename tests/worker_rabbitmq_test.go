package austro_os_test

import (
	"encoding/json"
	"testing"
	"time"

	"austro-os/internal/event"
	"austro-os/internal/worker"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

// testQueue declares a unique, transient queue for an isolated worker test.
// The declare arguments MUST match what the worker itself uses when it
// declares the same queue (durable=true, autoDelete=false); otherwise RabbitMQ
// raises PRECONDITION_FAILED for inequivalent arguments.
func testQueue(t *testing.T, ch *amqp.Channel) string {
	t.Helper()
	name := "austro.events.test." + uuid.NewString()
	_, err := ch.QueueDeclare(name, true, false, false, false, nil)
	require.NoError(t, err)
	return name
}

func openRabbit(t *testing.T) (*amqp.Connection, *amqp.Channel) {
	t.Helper()
	conn, err := amqp.Dial(getEnv().rabbitURL)
	require.NoError(t, err)
	ch, err := conn.Channel()
	require.NoError(t, err)
	t.Cleanup(func() { ch.Close(); conn.Close() })
	return conn, ch
}

// TestWorkerSuccessPath verifies the full RabbitMQ -> Worker happy path: a
// valid envelope is decoded, the handler runs, and the message is ACKed
// (queue drains to zero).
func TestWorkerSuccessPath(t *testing.T) {
	_, ch := openRabbit(t)
	q := testQueue(t, ch)

	handled := make(chan *event.UniversalEnvelope, 1)
	w := worker.NewWorker(worker.WorkerConfig{URL: getEnv().rabbitURL, QueueName: q},
		func(env *event.UniversalEnvelope) error {
			handled <- env
			return nil
		})
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	env := event.NewEnvelope()
	env.EventID = uuid.New()
	env.EventType = event.EventTypeCreated
	env.ActorType = "system"
	env.ActorID = uuid.New()
	env.TargetType = "workspace"
	env.TargetID = uuid.New()
	env.WorkspaceID = workspaceA
	env.TraceID = uuid.New()
	env.SpanID = uuid.New()
	env.ConstitutionalPrinciple = "Vision First"
	env.Outcome = "success"

	body, err := env.MarshalJSON()
	require.NoError(t, err)

	require.NoError(t, ch.Publish("", q, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		MessageId:   env.EventID.String(),
	}))

	select {
	case got := <-handled:
		require.Equal(t, env.EventID, got.EventID, "worker must decode and deliver the exact event")
		require.Equal(t, event.EventTypeCreated, got.EventType)
		require.Equal(t, env.ConstitutionalPrinciple, got.ConstitutionalPrinciple)
	case <-time.After(8 * time.Second):
		t.Fatal("worker did not process the valid message")
	}

	// Because the handler succeeded, the worker ACKs: the queue must drain.
	require.Eventually(t, func() bool {
		info, err := ch.QueueDeclarePassive(q, true, false, false, false, nil)
		if err != nil {
			return false
		}
		return info.Messages == 0
	}, 8*time.Second, 200*time.Millisecond, "queue must be drained after successful ACK")
}

// TestWorkerInvalidMessagePath verifies the worker's failure handling: a
// malformed payload is received, cannot be decoded, and is rejected safely
// without crashing the consumer or keeping the message in the queue.
func TestWorkerInvalidMessagePath(t *testing.T) {
	_, ch := openRabbit(t)
	q := testQueue(t, ch)

	w := worker.NewWorker(worker.WorkerConfig{URL: getEnv().rabbitURL, QueueName: q},
		func(env *event.UniversalEnvelope) error {
			// A malformed payload must never be delivered to the handler.
			t.Errorf("handler must not be invoked for undecodable payload")
			return nil
		})
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	require.NoError(t, ch.Publish("", q, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        []byte(`{this is not valid json`),
		MessageId:   "bad-msg",
	}))

	// The malformed message must be consumed (acked on the decode-error path)
	// and therefore must drain out of the queue rather than requeue forever.
	require.Eventually(t, func() bool {
		info, err := ch.QueueDeclarePassive(q, true, false, false, false, nil)
		if err != nil {
			return false
		}
		return info.Messages == 0
	}, 8*time.Second, 200*time.Millisecond, "malformed message must be consumed/rejected without getting stuck")
}

// TestWorkerContextPropagation checks that trace/span/correlation headers flow
// into the envelope during processing, mirroring the production worker.
func TestWorkerContextPropagation(t *testing.T) {
	_, ch := openRabbit(t)
	q := testQueue(t, ch)

	received := make(chan *event.UniversalEnvelope, 1)
	w := worker.NewWorker(worker.WorkerConfig{URL: getEnv().rabbitURL, QueueName: q},
		func(env *event.UniversalEnvelope) error {
			received <- env
			return nil
		})
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	env := event.NewEnvelope()
	env.EventType = event.EventTypeStarted
	env.ActorType = "system"
	env.ActorID = uuid.New()
	env.TargetType = "workspace"
	env.TargetID = uuid.New()
	env.ConstitutionalPrinciple = "Quality Over Speed"
	env.Outcome = "started"

	traceID := uuid.New()
	spanID := uuid.New()
	body, err := env.MarshalJSON()
	require.NoError(t, err)
	require.NoError(t, ch.Publish("", q, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        body,
		Headers:     amqp.Table{"trace_id": traceID.String(), "span_id": spanID.String()},
	}))

	select {
	case got := <-received:
		require.Equal(t, traceID, got.TraceID, "trace_id must propagate to the worker envelope")
		require.Equal(t, spanID, got.SpanID, "span_id must propagate to the worker envelope")
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for context-propagated event")
	}
}

// TestEnvelopeJSONRoundTrip is a pure unit check that a valid envelope with
// real UUIDs serializes and deserializes losslessly (no non-UUID test data).
func TestEnvelopeJSONRoundTrip(t *testing.T) {
	env := event.NewEnvelope()
	env.EventID = uuid.New()
	env.EventType = event.EventTypeCreated
	env.ActorType = "user"
	env.ActorID = uuid.New()
	env.TargetType = "team"
	env.TargetID = uuid.New()
	env.WorkspaceID = workspaceA
	env.ConstitutionalPrinciple = "Vision First"
	env.Outcome = "success"

	b, err := env.MarshalJSON()
	require.NoError(t, err)

	var back event.UniversalEnvelope
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, env.EventID, back.EventID)
	require.Equal(t, env.ConstitutionalPrinciple, back.ConstitutionalPrinciple)
	require.Equal(t, env.EventType, back.EventType)

	// The serialized event_id must be the real, non-nil UUID.
	var raw struct {
		EventID string `json:"event_id"`
	}
	require.NoError(t, json.Unmarshal(b, &raw))
	require.Equal(t, env.EventID.String(), raw.EventID, "event_id must serialize as the real UUID")
	require.NotEqual(t, "00000000-0000-0000-0000-000000000000", raw.EventID)
}
