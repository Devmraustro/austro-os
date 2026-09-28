package execution

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ExecutionStore is the workspace-scoped persistence port for AI executions.
// Every method takes the workspace explicitly and every implementation must
// bind app.current_workspace transaction-locally before touching a row, so the
// row-level security boundary is the enforcement layer even for callers whose
// credentials could otherwise see more.
//
// The lease methods are the heart of the runtime. Claim is the ONLY entry that
// increments attempt and assigns the lease; Heartbeat only renews the lease
// expiry; every mutation after Claim re-asserts worker identity and lease
// generation and fails with ErrStaleLease when the row moved on. Stale-worker
// fencing is therefore enforced in SQL, not by application discipline.
type ExecutionStore interface {
	// AdmissionAdmit is the single gate through which executions enter the
	// system. It runs SERIALIZABLE, takes a per-employee advisory transaction
	// lock, resolves idempotency and capacity, and only then inserts the
	// pending execution (attempt 0, null lease) with its first outstanding
	// retry row. It is the ONLY insert path, so capacity never has two
	// implementations to drift apart.
	AdmissionAdmit(ctx context.Context, req AdmitRequest, requestHash string) (AdmitResult, error)

	// Get returns one execution inside the caller's workspace.
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Execution, error)

	// GetByIdempotencyKey resolves an execution by idempotency key inside the
	// caller's workspace. A missing row is ErrNotFound.
	GetByIdempotencyKey(ctx context.Context, workspaceID uuid.UUID, key string) (*Execution, error)

	// Capacity is the concurrency ceiling configured for the AI employee.
	Capacity(ctx context.Context, workspaceID, aiEmployeeID uuid.UUID) (int, error)

	// ActiveCount is the number of pk execution rows for the employee in
	// (pending, running) — the two states that still consume capacity.
	ActiveCount(ctx context.Context, workspaceID, aiEmployeeID uuid.UUID) (int, error)

	// GetDue lists due execution ids inside one workspace: pending and either
	// never scheduled or with next_retry_at in the past.
	GetDue(ctx context.Context, workspaceID uuid.UUID, limit int) ([]uuid.UUID, error)

	// GetStale lists experimental execution ids whose lease expired while
	// running, oldest first. ReclaimStale performs the actual takeover.
	GetStale(ctx context.Context, workspaceID uuid.UUID, limit int) ([]uuid.UUID, error)

	// Claim atomically moves one due execution to running under a new lease,
	// bumps attempt exactly once and flips its outstanding retry to claimed
	// with the lockstep generation. ErrNoRows ever becoming a successful claim
	// is guaranteed by returning the fenced row from the same statement.
	Claim(ctx context.Context, workspaceID, id uuid.UUID, workerID string, leaseTTL time.Duration) (*FencedExecution, error)

	// ReclaimStale transfers a running execution whose lease expired to a new
	// owner, bumping the generation exactly once. It returns the prior error
	// classification (nil/absent means the worker crashed without persisting
	// one, which recovery classifies as worker_lease_expired).
	ReclaimStale(ctx context.Context, workspaceID, id uuid.UUID, workerID string, leaseTTL time.Duration) (*FencedExecution, error)

	// Heartbeat renews only the lease expiry. RowsAffected must be exactly 1;
	// 0 means ownership moved and the worker must stop.
	Heartbeat(ctx context.Context, f *FencedExecution, leaseTTL time.Duration) error

	// PersistError records the classified failure on the running execution
	// before any terminal or reschedule decision is made.
	PersistError(ctx context.Context, f *FencedExecution, code ErrorCode, retryable bool, detail string) error

	// Complete marks the execution completed with its result, resolves the
	// outstanding retry as processed/success (worker cleared) and clears the
	// lease, all in one fenced transaction.
	Complete(ctx context.Context, f *FencedExecution, result []byte) error

	// FailAndDLQ marks the execution failed, resolves the outstanding retry as
	// processed/failed (worker cleared) and appends the DLQ entry, all in one
	// fenced transaction.
	FailAndDLQ(ctx context.Context, f *FencedExecution, code ErrorCode, detail string, snapshot []byte) error

	// Reschedule returns the execution to pending under a fresh lease-free
	// state, resolves the outstanding retry as processed/failed (worker
	// cleared) and inserts the next pending retry row, all in one fenced
	// transaction. nextRetryAt gates the next claim; seq is the next retry
	// sequence number.
	Reschedule(ctx context.Context, f *FencedExecution, nextRetryAt time.Time, seq int) error

	// RecordDispatch appends to retry_dispatch_log only while the retry row is
	// still outstanding for the same worker and generation (a stale dispatch
	// writes nothing), and flips the retry to dispatched on publish success.
	RecordDispatch(ctx context.Context, workspaceID, retryID uuid.UUID, workerID string, generation int64, success bool) error

	// Cancel moves a pending/running execution to cancelled under the
	// workspace lock. It is idempotent: an execution already in a terminal
	// state yields nil; a missing one yields ErrNotFound.
	Cancel(ctx context.Context, workspaceID, id uuid.UUID) error
}

// AdmitRequest is the admission input. IdempotencyKey is optional; when set it
// is combined with requestHash so a repeat call returns the existing execution
// and an altered payload is rejected as conflicting.
type AdmitRequest struct {
	WorkspaceID    uuid.UUID
	AIEmployeeID   uuid.UUID
	TaskID         *uuid.UUID
	MaxAttempts    int
	IdempotencyKey string
	// Context is the sanitized workspace snapshot captured at admission. When
	// non-nil the store inserts the execution_contexts row in the same admission
	// transaction, so a persisted execution always has its context materialized
	// and the worker never re-reads raw task or employee data at run time.
	Context *MaterializedRequest
}

// AdmitResult discriminates an accepted insert from an idempotent hit.
type AdmitResult struct {
	// Execution is either the newly created row or the pre-existing one.
	Execution *Execution
	// Created reports whether this call performed the insert.
	Created bool
	// Completed reports whether the resolved execution is terminal/completed,
	// which the HTTP layer maps to a 200-with-result instead of 202.
	Completed bool
}
