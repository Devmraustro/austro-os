package execution

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PublishMessage is the outbound unit of work: one claimed retry of one
// execution, fenced by the lease owner and generation. It carries identity
// only — the worker materials the request body from the workspace database —
// so the dispatcher never handles prompt payloads.
//
// LeaseOwner MANDATORY for a real transport: it is the lease fingerprint the
// claim stored (the dispatcher's NewWorkerID), and the worker re-asserts it on
// every fenced write. Omitting it forces the worker to fall back to its own
// process identity, which in the two-process topology (API dispatcher + worker
// process) can never match the stored owner and makes every fenced write a
// stale-lease error.
type PublishMessage struct {
	WorkspaceID    uuid.UUID
	ExecutionID    uuid.UUID
	AIEmployeeID   uuid.UUID
	TaskID         *uuid.UUID
	Attempt        int
	MaxAttempts    int
	RetryID        uuid.UUID
	Generation     int64
	LeaseExpiresAt time.Time
	LeaseOwner     string
}

// Publisher transports a claimed retry to a consumer. Publish must be
// at-least-once: exactly-once is not required and the fenced RecordDispatch
// step makes redundant publishes harmless.
type Publisher interface {
	Publish(ctx context.Context, msg PublishMessage) error
}
