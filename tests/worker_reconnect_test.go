package austro_os_test

import (
	"testing"
	"time"

	"austro-os/internal/event"
	"austro-os/internal/worker"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

// TestWorkerReconnectsAfterConnectionLoss proves the production worker does not
// silently stop consuming when its broker connection is dropped: the supervisor
// reconnects with backoff, re-registers the consumer on the same queue, and
// keeps processing messages. Without the reconnect logic the consumer channel
// closes indefinitely, a real operational failure mode.
func TestWorkerReconnectsAfterConnectionLoss(t *testing.T) {
	_, ch := openRabbit(t)
	q := testQueue(t, ch)

	handled := make(chan *event.UniversalEnvelope, 4)
	w := worker.NewWorker(worker.WorkerConfig{URL: getEnv().rabbitURL, QueueName: q},
		func(env *event.UniversalEnvelope) error {
			handled <- env
			return nil
		}, worker.WithReconnectDelay(500*time.Millisecond))
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	publish := func() {
		env := event.NewEnvelope()
		env.EventID = uuid.New()
		env.EventType = event.EventTypeCreated
		env.ActorType = "system"
		env.TargetType = "workspace"
		env.TargetID = uuid.New()
		env.WorkspaceID = workspaceA
		env.TraceID = uuid.New()
		env.SpanID = uuid.New()
		body, err := env.MarshalJSON()
		require.NoError(t, err)
		require.NoError(t, ch.Publish("", q, false, false, amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
			MessageId:   env.EventID.String(),
		}))
	}

	// Baseline: the worker consumes before the connection loss.
	publish()
	select {
	case <-handled:
	case <-time.After(8 * time.Second):
		t.Fatal("worker did not process the message before connection loss")
	}

	// Drop the broker connection. The supervisor must redial and re-register.
	w.CloseBrokerConnection()
	require.Eventually(t, func() bool {
		info, err := ch.QueueInspect(q)
		if err != nil {
			return false
		}
		return info.Consumers == 1
	}, 15*time.Second, 250*time.Millisecond, "worker must re-register its consumer after connection loss")

	// A message published after the recovery must still be handled (it is held
	// by the durable queue until the consumer returns).
	publish()
	select {
	case <-handled:
	case <-time.After(8 * time.Second):
		t.Fatal("worker did not process a message after reconnection")
	}
}
