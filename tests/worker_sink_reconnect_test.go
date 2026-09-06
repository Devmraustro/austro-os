package austro_os_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"austro-os/infrastructure/rabbitmq"
	"austro-os/internal/event"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestEventSinkReconnectsAfterConnectionLoss proves the self-supervised event
// sink (used by the worker and the API for pipeline-advance cascade publishing)
// keeps publishing after the broker force-closes its connection. A static
// channel would silently drop every downstream pipeline.<stage> event, stalling
// the cascade mid-flight -- the observed production failure mode.
func TestEventSinkReconnectsAfterConnectionLoss(t *testing.T) {
	_, ch := openRabbit(t)
	q := testQueue(t, ch)

	sink, err := rabbitmq.NewReconnectingSink(getEnv().rabbitURL, q)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sink.Close()) })

	es := rabbitmq.NewPublishEventSink(sink)

	msgs, err := ch.Consume(q, "", true, false, false, false, nil)
	require.NoError(t, err)

	delivered := func(id uuid.UUID) bool {
		select {
		case d := <-msgs:
			var env event.UniversalEnvelope
			if json.Unmarshal(d.Body, &env) != nil {
				return false
			}
			return env.TargetID == id
		case <-time.After(10 * time.Second):
			return false
		}
	}

	// Baseline: the sink publishes before any connection loss.
	first := uuid.New()
	require.NoError(t, es.Publish(context.Background(), "publication.created", first, uuid.MustParse(workspaceA), uuid.NewString(), uuid.NewString()))
	require.True(t, delivered(first), "sink must publish before connection loss")

	// Force-close the sink's connection. Supervision must redial and keep the
	// sink hot; a subsequent publish must land on the same durable queue.
	require.NoError(t, sink.BreakConnection())

	second := uuid.New()
	require.Eventually(t, func() bool {
		if err := es.Publish(context.Background(), "publication.created", second, uuid.MustParse(workspaceA), uuid.NewString(), uuid.NewString()); err != nil {
			// Still inside the redial/backoff window.
			return false
		}
		return true
	}, 15*time.Second, 200*time.Millisecond, "sink must reconnect and publish again")
	require.True(t, delivered(second), "event published after reconnection must be delivered")
}
