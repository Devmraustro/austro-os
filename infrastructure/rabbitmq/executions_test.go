package rabbitmq

import (
	"encoding/json"
	"testing"
	"time"

	"austro-os/internal/event"
	"austro-os/internal/execution"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestExecutionEnvelopeRoundTrip proves the dispatch envelope carries the exact
// fenced identity: a consumer decoding Details reproduces the original
// PublishMessage, including the lease expiry and generation that fence the run.
func TestExecutionEnvelopeRoundTrip(t *testing.T) {
	lease := time.Now().UTC().Add(90 * time.Second)
	taskID := uuid.New()
	msg := execution.PublishMessage{
		WorkspaceID:    uuid.New(),
		ExecutionID:    uuid.New(),
		AIEmployeeID:   uuid.New(),
		TaskID:         &taskID,
		Attempt:        2,
		MaxAttempts:    3,
		RetryID:        uuid.New(),
		Generation:     7,
		LeaseExpiresAt: lease,
		LeaseOwner:     "api-host-42",
	}

	env, hdrs, err := executionEnvelope(msg)
	require.NoError(t, err)
	require.Equal(t, "execution", env.TargetType)
	require.Equal(t, msg.ExecutionID, env.TargetID)
	require.Equal(t, msg.WorkspaceID.String(), env.WorkspaceID)
	require.Equal(t, event.EventTypeCreated, env.EventType)
	require.Equal(t, msg.RetryID.String(), hdrs["retry_id"])
	require.Equal(t, int64(7), hdrs["generation"])

	var back execution.PublishMessage
	require.NoError(t, json.Unmarshal(env.Details, &back))
	require.Equal(t, msg, back)
	require.Equal(t, lease, back.LeaseExpiresAt, "lease expiry must survive the dispatch")
	require.Equal(t, int64(7), back.Generation)
	require.NotNil(t, back.TaskID)
	require.Equal(t, taskID, *back.TaskID)
}

// TestExecutionPublisherSatisfiesPort is a compile-time proof the adapter
// fulfils the dispatcher transport contract.
func TestExecutionPublisherSatisfiesPort(t *testing.T) {
	var p execution.Publisher = NewExecutionPublisher(nil)
	require.NotNil(t, p)
}