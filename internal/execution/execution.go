// Package execution provides the AI execution domain for the Creator pipeline.
//
// An execution is a single run of a workspace task by an AI employee through
// the LLM runtime. It is workspace-scoped, explicit in its lifecycle, leased to
// exactly one worker at a time, retried up to a bounded number of times, and
// dead-lettered when retries are exhausted or the error is not retryable.
//
// The package defines the execution aggregate, the retry aggregate, a
// workspace-scoped persistence port (ExecutionStore), and the error
// classification taxonomy workers and recovery use to decide which failures
// are retryable. It imports no infrastructure package (the same dependency
// direction the task domain follows).
package execution

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"austro-os/internal/ai"

	"github.com/google/uuid"
)

// Sentinel errors reported across the execution boundary. Callers use
// errors.Is against these; the database adapter wraps its own internals with
// them so a store-level failure still maps onto the domain surface.
var (
	ErrInvalidInput      = errors.New("execution: invalid input")
	ErrNotFound          = errors.New("execution: not found")
	ErrWorkspaceMismatch = errors.New("execution: workspace mismatch")
	ErrStatus            = errors.New("execution: invalid status")
	ErrCapacityExceeded  = errors.New("execution: ai employee concurrency capacity exceeded")
	ErrIdempotencyKey    = errors.New("execution: idempotency key conflict")
	ErrStaleLease        = errors.New("execution: lease lost to another worker")
	ErrNotRetryable      = errors.New("execution: error is not retryable")
)

// Status is an execution lifecycle stage. Statuses are stored as text and
// validated here; the database CHECK constraint mirrors this list and a test
// pins the two to stay identical.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// ValidStatus reports whether s is a recognized lifecycle status.
func ValidStatus(s Status) bool {
	switch s {
	case StatusPending, StatusRunning, StatusCompleted, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Terminal reports whether the status is final: nothing may transition it and
// no lease may ever be taken out on it again.
func Terminal(s Status) bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// RetryState is the lifecycle stage of a single retry deliverable.
type RetryState string

const (
	RetryPending    RetryState = "pending"    // schedulable, worker unknown
	RetryClaimed    RetryState = "claimed"    // taken out of the queue by a worker
	RetryDispatched RetryState = "dispatched" // ack published to the broker
	RetryProcessed  RetryState = "processed"  // resolved: success, failed or cancelled
)

// ValidRetryState reports whether s is a recognized retry state.
func ValidRetryState(s RetryState) bool {
	switch s {
	case RetryPending, RetryClaimed, RetryDispatched, RetryProcessed:
		return true
	}
	return false
}

// RetryOutcome is the resolution of a processed retry.
type RetryOutcome string

const (
	RetrySuccess   RetryOutcome = "success"
	RetryFailed    RetryOutcome = "failed"
	RetryCancelled RetryOutcome = "cancelled"
)

// ValidRetryOutcome reports whether o is recognized.
func ValidRetryOutcome(o RetryOutcome) bool {
	switch o {
	case RetrySuccess, RetryFailed, RetryCancelled:
		return true
	}
	return false
}

// ErrorCode is the persisted classification of a failed attempt. The numeric
// values are part of the storage contract: ai_executions.last_error_code and
// ai_execution_dlq.error_code carry them, and down-stream tooling reads them,
// so the numbers never change even when new codes are added.
type ErrorCode int

const (
	// CodeNone is the absence of an error (not retryable, terminal success).
	CodeNone ErrorCode = iota // 0
	// CodeDeadlineExceeded is context or deadline expiry (retryable).
	CodeDeadlineExceeded // 1
	// CodeProviderTimeout is a provider-side call timeout (retryable).
	CodeProviderTimeout // 2
	// CodeRateLimit is a provider rate limit (retryable).
	CodeRateLimit // 3
	// CodeUnavailable is a transient infrastructure or provider outage (retryable).
	CodeUnavailable // 4
	// CodeCanceled is a cancellation (not retryable).
	CodeCanceled // 5
	// CodeNotFound is a missing row, normally sql.ErrNoRows (not retryable by default).
	CodeNotFound // 6
	// CodeUnknown is an unclassified error (not retryable).
	CodeUnknown // 7
	// CodeWorkerLeaseExpired is a worker that crashed without persisting a
	// classified error; recovery classifies the next_retry slot from it
	// (retryable subject to max_attempts).
	CodeWorkerLeaseExpired // 8
)

// Retryable reports whether a code may be retried within max_attempts.
func (c ErrorCode) Retryable() bool {
	switch c {
	case CodeDeadlineExceeded, CodeProviderTimeout, CodeRateLimit, CodeUnavailable, CodeWorkerLeaseExpired:
		return true
	}
	return false
}

// String renders the code as a human-readable name.
func (c ErrorCode) String() string {
	switch c {
	case CodeNone:
		return "none"
	case CodeDeadlineExceeded:
		return "deadline_exceeded"
	case CodeProviderTimeout:
		return "provider_timeout"
	case CodeRateLimit:
		return "rate_limit"
	case CodeUnavailable:
		return "unavailable"
	case CodeCanceled:
		return "canceled"
	case CodeNotFound:
		return "not_found"
	case CodeUnknown:
		return "unknown"
	case CodeWorkerLeaseExpired:
		return "worker_lease_expired"
	}
	return "unknown"
}

// ClassifiedError couples a concrete cause with its persisted classification.
// It implements Unwrap and Is so errors.Is classifies a wrapped cause exactly
// once and downstream errors.Is against a code sentinel works through layers.
type ClassifiedError struct {
	Code  ErrorCode
	Retry bool
	Cause error
}

// NewClassifiedError builds a classified error.
func NewClassifiedError(code ErrorCode, cause error) *ClassifiedError {
	return &ClassifiedError{Code: code, Retry: code.Retryable(), Cause: cause}
}

// Error implements error.
func (e *ClassifiedError) Error() string {
	if e.Cause == nil {
		return "execution: " + e.Code.String()
	}
	return "execution: " + e.Code.String() + ": " + e.Cause.Error()
}

// Unwrap exposes the underlying cause for errors.Is and errors.As.
func (e *ClassifiedError) Unwrap() error { return e.Cause }

// Is lets errors.Is match a *ClassifiedError against a plain code sentinel,
// e.g. errors.Is(err, ErrNotRetryable) for a non-retryable classification.
func (e *ClassifiedError) Is(target error) bool {
	if target == ErrNotRetryable {
		return !e.Retry
	}
	return false
}

// ErrWorkerLeaseExpired is the classified sentinel recovery uses for a crash
// without a persisted error. It is retryable.
var ErrWorkerLeaseExpired = &ClassifiedError{Code: CodeWorkerLeaseExpired, Retry: true}

// ErrNotFoundClassified is the classification of sql.ErrNoRows (not retryable
// by default). Recovery guards on it so a vanished row never becomes an
// infinite retry.
var ErrNotFoundClassified = &ClassifiedError{Code: CodeNotFound, Retry: false}

// Classify maps a concrete cause onto the persisted taxonomy. The standard
// library surface (context, sql, net) is classified conservatively; provider
// SDK wrappers that raise typed sentinels (ai.ErrUsageLimitExceeded) land on
// the matching retryable code. Everything else is CodeUnknown (not retryable)
// rather than guessed retryable.
func Classify(cause error) *ClassifiedError {
	if cause == nil {
		return &ClassifiedError{Code: CodeNone}
	}
	switch {
	case errors.Is(cause, sql.ErrNoRows):
		return &ClassifiedError{Code: CodeNotFound, Retry: false, Cause: cause}
	case errors.Is(cause, context.Canceled):
		return &ClassifiedError{Code: CodeCanceled, Retry: false, Cause: cause}
	case errors.Is(cause, context.DeadlineExceeded):
		return &ClassifiedError{Code: CodeDeadlineExceeded, Retry: true, Cause: cause}
	case errors.Is(cause, ai.ErrUsageLimitExceeded):
		return &ClassifiedError{Code: CodeRateLimit, Retry: true, Cause: cause}
	case errors.Is(cause, ErrWorkerLeaseExpired):
		return &ClassifiedError{Code: CodeWorkerLeaseExpired, Retry: true, Cause: cause}
	case errors.Is(cause, net.ErrClosed), errors.Is(cause, io.EOF),
		errors.Is(cause, syscall.ECONNREFUSED), errors.Is(cause, syscall.ECONNRESET),
		errors.Is(cause, syscall.ETIMEDOUT):
		return &ClassifiedError{Code: CodeUnavailable, Retry: true, Cause: cause}
	}
	var netErr net.Error
	if errors.As(cause, &netErr) && netErr.Timeout() {
		return &ClassifiedError{Code: CodeProviderTimeout, Retry: true, Cause: cause}
	}
	return &ClassifiedError{Code: CodeUnknown, Retry: false, Cause: cause}
}

// Execution is the persistent execution aggregate. Lease fields are managed by
// the claim/reclaim/heartbeat machinery and must never be set by admission:
// a pending row always carries a null lease. Attempt counts only the attempts
// actually started (claim increments), never admissions.
type Execution struct {
	ID              uuid.UUID  `json:"id"`
	WorkspaceID     uuid.UUID  `json:"workspace_id"`
	AIEmployeeID    uuid.UUID  `json:"ai_employee_id"`
	TaskID          *uuid.UUID `json:"task_id,omitempty"`
	Status          Status     `json:"status"`
	Attempt         int        `json:"attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	IdempotencyKey  *string    `json:"idempotency_key,omitempty"`
	RequestHash     string     `json:"request_hash"`
	LeaseOwner      *string    `json:"lease_owner,omitempty"`
	LeaseGeneration int64      `json:"lease_generation"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	NextRetryAt     *time.Time `json:"next_retry_at,omitempty"`
	LastErrorCode   ErrorCode  `json:"last_error_code,omitempty"`
	Retryable       *bool      `json:"retryable,omitempty"`
	ErrorDetail     *string    `json:"error_detail,omitempty"`
	ResultJSON      []byte     `json:"result_json,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	FailedAt        *time.Time `json:"failed_at,omitempty"`
	CancelledAt     *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Retry is one retry deliverable of an execution. generation must track the
// execution's lease_generation for the worker that raised the retry, and a
// processed row always has worker_id null.
type Retry struct {
	ID           uuid.UUID     `json:"id"`
	ExecutionID  uuid.UUID     `json:"execution_id"`
	WorkspaceID  uuid.UUID     `json:"workspace_id"`
	RetrySeq     int           `json:"retry_seq"`
	State        RetryState    `json:"state"`
	Outcome      *RetryOutcome `json:"outcome,omitempty"`
	WorkerID     *string       `json:"worker_id,omitempty"`
	Generation   int64         `json:"generation"`
	NextRetryAt  *time.Time    `json:"next_retry_at,omitempty"`
	ClaimedAt    *time.Time    `json:"claimed_at,omitempty"`
	DispatchedAt *time.Time    `json:"dispatched_at,omitempty"`
	CompletedAt  *time.Time    `json:"completed_at,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
}

// FencedExecution is a claimed execution: the worker that ran the claim owns
// it exclusively until heartbeat or lease expiry. Every later worker mutation
// must re-assert owner+generation so a stale worker can never write.
//
// LastErrorCode/Retryable carry the prior failure classification for a
// reclaimed execution: nil means the prior worker crashed before persisting
// one, which recovery classifies as worker_lease_expired.
type FencedExecution struct {
	ExecutionID     uuid.UUID
	WorkspaceID     uuid.UUID
	LeaseOwner      string
	LeaseGeneration int64
	LeaseExpiresAt  time.Time
	Attempt         int
	MaxAttempts     int
	RetryID         uuid.UUID
	AIEmployeeID    uuid.UUID
	LastErrorCode   *ErrorCode
	Retryable       *bool
	NextRetryAt     *time.Time
}

// ownerMatches is a domain-side guard used by the stores: a mutation may only
// continue when the caller's worker identity equals the lease owner.
func ownerMatches(exec *Execution, workerID string) bool {
	return exec.LeaseOwner != nil && *exec.LeaseOwner == workerID
}
