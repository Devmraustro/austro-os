package rabbitmq

import (
	"testing"

	"austro-os/internal/config"

	"github.com/stretchr/testify/require"
)

// TestDefaultExecutionQueueMatchesConfigDefault pins the two declarations of the
// execution queue name to each other. The dispatcher resolves the name from
// configuration and the worker resolves it from configuration, so the only thing
// that keeps the two processes talking is that both fall back to the same
// literal. The literal is written twice on purpose — config cannot import this
// package without a cycle — which means nothing but this test stops the two
// copies from drifting apart into a deployment where the dispatcher publishes
// into a queue nothing consumes and both processes report healthy.
func TestDefaultExecutionQueueMatchesConfigDefault(t *testing.T) {
	require.Equal(t, config.DefaultExecutionQueue, ExecutionQueue)
}
