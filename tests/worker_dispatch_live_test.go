package austro_os_test

import (
	"encoding/json"
	"testing"
	"time"

	"austro-os/internal/event"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

// TestWorkerAdvancesPipelineFromEventToComplete proves the running worker
// container (composed runtime with stub backends) dispatches pipeline events:
// a seeded pipeline.research message must cascade research -> script -> review
// -> publish -> complete through the real RabbitMQ queue and PostgreSQL store.
// It requires the worker service to be up (compose depends_on) and the default
// austro.events queue. Stub mode guarantees no external AI/publish calls.
func TestWorkerAdvancesPipelineFromEventToComplete(t *testing.T) {
	db, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer db.Close()

	wsID := uuid.New()
	pipeID := uuid.New()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM pipelines WHERE id=$1`, pipeID)
		_, _ = db.Exec(`DELETE FROM workspaces WHERE id=$1`, wsID)
	})

	_, err = db.Exec(`INSERT INTO workspaces (id, name) VALUES ($1,$2)`, wsID, "dispatch-test")
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO pipelines (id, workspace_id, stage, status, trace_id, created_at, updated_at)
		 VALUES ($1,$2,'research','created',$3, NOW(), NOW())`,
		pipeID, wsID, "live-dispatch")
	require.NoError(t, err)

	_, ch := openRabbit(t)
	// Ensure the real worker queue exists before publishing (idempotent; the
	// worker declares it with identical arguments on startup).
	_, err = ch.QueueDeclare("austro.events", true, false, false, false, nil)
	require.NoError(t, err)

	details, err := json.Marshal(map[string]string{"event": "pipeline.research", "stage": "research"})
	require.NoError(t, err)
	env := event.NewEnvelope()
	env.EventID = uuid.New()
	env.EventType = event.EventTypeCreated
	env.ActorType = "system"
	env.TargetType = "pipeline"
	env.TargetID = pipeID
	env.WorkspaceID = wsID.String()
	env.TraceID = uuid.New()
	env.SpanID = uuid.New()
	env.ConstitutionalPrinciple = "Obedience"
	env.Outcome = "success"
	env.Details = details

	body, err := env.MarshalJSON()
	require.NoError(t, err)
	require.NoError(t, ch.Publish("", "austro.events", false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Body:         body,
		MessageId:    env.EventID.String(),
		Headers: amqp.Table{
			"event_type":   "created",
			"workspace_id": wsID.String(),
			"target_type":  "pipeline",
			"trace_id":     env.TraceID.String(),
			"span_id":      env.SpanID.String(),
		},
	}))

	var stage, status string
	// The worker must stop at the durable human gate rather than inventing
	// approval. Wait for the review handoff and then persist the same approval
	// evidence that the named API command writes. The API-level test covers the
	// HTTP/RBAC path; this live worker test focuses on consuming the resulting
	// review_approved event and completing the remaining cascade.
	require.Eventually(t, func() bool {
		err := db.QueryRow(`SELECT stage, status FROM pipelines WHERE id=$1`, pipeID).Scan(&stage, &status)
		return err == nil && stage == "review" && status == "awaiting_approval"
	}, 90*time.Second, 1*time.Second, "worker must persist the review handoff")

	var publicationID uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT publication_id FROM pipelines WHERE id=$1`, pipeID).Scan(&publicationID))
	now := time.Now().UTC()
	_, err = db.Exec(`UPDATE publications SET status='approved', approved_by='worker-test-admin', approved_at=$1 WHERE id=$2`, now, publicationID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE pipelines SET status='approved', approved_by='worker-test-admin', approved_at=$1 WHERE id=$2`, now, pipeID)
	require.NoError(t, err)

	approvalDetails, err := json.Marshal(map[string]string{"event": "pipeline.review_approved", "stage": "review"})
	require.NoError(t, err)
	approvalEnv := env
	approvalEnv.EventID = uuid.New()
	approvalEnv.Details = approvalDetails
	approvalBody, err := approvalEnv.MarshalJSON()
	require.NoError(t, err)
	require.NoError(t, ch.Publish("", "austro.events", false, false, amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: approvalBody,
		MessageId: approvalEnv.EventID.String(),
		Headers: amqp.Table{"event_type": "created", "workspace_id": wsID.String(), "target_type": "pipeline", "trace_id": approvalEnv.TraceID.String(), "span_id": approvalEnv.SpanID.String()},
	}))

	// The remaining publish -> complete cascade is entirely worker-owned.
	require.Eventually(t, func() bool {
		err := db.QueryRow(`SELECT stage, status FROM pipelines WHERE id=$1`, pipeID).Scan(&stage, &status)
		return err == nil && stage == "complete" && status == "done"
	}, 90*time.Second, 1*time.Second,
		"worker must advance the approved pipeline through the message loop to complete")
}
