package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"austro-os/internal/config"
)

// TestQueueForDefaultsToAustroEvents proves the worker entrypoint resolves the
// events queue name exactly as the API entrypoint does: the configured name
// when set, otherwise the default austro.events. The worker and API must agree
// on the default so a worker never consumes a server-named queue while the API
// publishes to austro.events when AUSTRO_RABBITMQ_QUEUE is unset.
func TestQueueForDefaultsToAustroEvents(t *testing.T) {
	require.Equal(t, "austro.events", queueFor(&config.Config{}))

	cfg := &config.Config{RabbitMQQueue: "austro.events.production"}
	require.Equal(t, "austro.events.production", queueFor(cfg))
}
