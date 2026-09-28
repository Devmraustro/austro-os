package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"austro-os/internal/ai"

	"github.com/google/uuid"
)

// RunOutcome is what a worker Run call decided, so consumers can log and the
// transport layer can settle the broker message accordingly.
type RunOutcome string

const (
	// OutcomeCompleted: result persisted, lease released (message ackable).
	OutcomeCompleted RunOutcome = "completed"
	// OutcomeScheduled: classified failure persisted and retry scheduled.
	OutcomeScheduled RunOutcome = "scheduled"
	// OutcomeDeadLettered: non-retryable or exhausted failure, DLQ appended.
	OutcomeDeadLettered RunOutcome = "deadlettered"
	// OutcomeAbandoned: ownership moved (heartbeat lost) or store failure; no
	// terminal write was made, recovery owns the row now.
	OutcomeAbandoned RunOutcome = "abandoned"
)

// Worker runs one delivered attempt: it materializes the sanitized request,
// calls the provider under a deadline that never exceeds the lease, renews the
// lease by heartbeat, and finalizes the execution — completed, rescheduled or
// dead-lettered. It is intentionally stateless and driven by the broker
// consumer; the store board is the source of truth.
type Worker struct {
	store             ExecutionStore
	mat               Materializer
	prov              ai.Provider
	log               *slog.Logger
	workerID          string
	leaseTTL          time.Duration
	heartbeatInterval time.Duration
	executionWindow   time.Duration
	baseDelay         time.Duration
	maxDelay          time.Duration
	now               func() time.Time
}

// NewWorker wires a worker to its ports. leaseTTL must match what the
// dispatcher granted; heartbeatInterval should be well under leaseTTL (e.g.
// leaseTTL/3) so a healthy worker never looks stale. executionWindow caps the
// non-heartbeat provider call budget.
func NewWorker(store ExecutionStore, mat Materializer, prov ai.Provider, workerID string, leaseTTL, heartbeatInterval, executionWindow, baseDelay, maxDelay time.Duration) *Worker {
	return &Worker{
		store:             store,
		mat:               mat,
		prov:              prov,
		log:               slog.Default(),
		workerID:          workerID,
		leaseTTL:          leaseTTL,
		heartbeatInterval: heartbeatInterval,
		executionWindow:   executionWindow,
		baseDelay:         baseDelay,
		maxDelay:          maxDelay,
		now:               time.Now,
	}
}

// Run executes the attempt described by msg. The returned outcome records the
// write decision; the error is reserved for cases where no sane decision could
// be persisted (store failure, invalid message).
func (w *Worker) Run(ctx context.Context, msg PublishMessage) (RunOutcome, error) {
	if msg.WorkspaceID == uuid.Nil || msg.ExecutionID == uuid.Nil || msg.RetryID == uuid.Nil {
		return OutcomeDeadLettered, ErrInvalidInput
	}
	f := fencedFrom(msg, w.workerID)

	// The provider call never outlives the lease and never outlives the
	// configured execution window either; the heartbeat keeps enforcing the
	// first while we are alive.
	deadline := msg.LeaseExpiresAt
	if d2 := w.now().Add(w.executionWindow); d2.Before(deadline) {
		deadline = d2
	}
	runCtx, cancelRun := context.WithDeadline(ctx, deadline)
	defer cancelRun()

	materialized, err := w.mat.Materialize(ctx, msg.WorkspaceID, msg.ExecutionID)
	if err != nil {
		return OutcomeAbandoned, fmt.Errorf("materialize execution %s: %w", msg.ExecutionID, err)
	}

	// The heartbeat goroutine cancels the run the instant ownership moves: a
	// stale worker must stop writing and let recovery reclaim the lease.
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		t := time.NewTicker(w.heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := w.store.Heartbeat(hbCtx, f, w.leaseTTL); err != nil {
					w.log.Error("worker heartbeat lost", "execution", f.ExecutionID, "error", err)
					cancelRun() // abandon the provider call, do not write
					return
				}
			}
		}
	}()

	res, callErr := w.prov.Complete(runCtx, ai.CompletionRequest{
		Scope:       ai.Scope{WorkspaceID: msg.WorkspaceID.String()},
		Instruction: w.instruction(materialized),
	})
	stopHeartbeat()

	// Ownership moved while we were talking to the provider: leave the row to
	// recovery. This must be checked before classifying so a canceled context
	// is never turned into a terminal write by a worker that no longer owns it.
	if errors.Is(runCtx.Err(), context.Canceled) {
		return OutcomeAbandoned, nil
	}

	if callErr != nil {
		return w.finalizeFailure(ctx, f, materialized, callErr)
	}

	payload, err := json.Marshal(res)
	if err != nil {
		return w.finalizeFailure(ctx, f, materialized, err)
	}
	if err := w.store.Complete(ctx, f, payload); err != nil {
		return OutcomeAbandoned, err
	}
	return OutcomeCompleted, nil
}

// finalizeFailure persists the classification and retries proactively (the
// truthful durability model: PersistError first, then Reschedule or DLQ), or
// fails the row to the DLQ. The two calls share the same fence, so a stale
// worker cannot reach this path.
func (w *Worker) finalizeFailure(ctx context.Context, f *FencedExecution, materialized *MaterializedRequest, cause error) (RunOutcome, error) {
	cl := Classify(cause)
	detail := cause.Error()

	if err := w.store.PersistError(ctx, f, cl.Code, cl.Retry, detail); err != nil {
		return OutcomeAbandoned, err
	}
	if cl.Retry && f.Attempt < f.MaxAttempts {
		next := w.now().Add(RetryDelay(f.Attempt, w.baseDelay, w.maxDelay))
		if err := w.store.Reschedule(ctx, f, next, f.Attempt+1); err != nil {
			return OutcomeAbandoned, err
		}
		return OutcomeScheduled, nil
	}
	if err := w.store.FailAndDLQ(ctx, f, cl.Code, detail, w.snapshot(materialized)); err != nil {
		return OutcomeAbandoned, err
	}
	return OutcomeDeadLettered, nil
}

// instruction assembles the sanitized prompt the provider sees. Secrets were
// already excluded at admission; the assembler itself adds nothing unvetted.
func (w *Worker) instruction(m *MaterializedRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, %s.", m.AIEmployeeName, m.AIEmployeeRole)
	if m.DepartmentName != "" {
		fmt.Fprintf(&b, " Department: %s.", m.DepartmentName)
	}
	if m.TeamName != "" {
		fmt.Fprintf(&b, " Team: %s.", m.TeamName)
	}
	if len(m.Capabilities) > 0 {
		fmt.Fprintf(&b, " Capabilities: %s.", strings.Join(m.Capabilities, ", "))
	}
	if m.TaskTitle != "" {
		fmt.Fprintf(&b, "\nTask: %s.", m.TaskTitle)
	}
	if m.TaskDescription != "" {
		fmt.Fprintf(&b, "\n%s", m.TaskDescription)
	}
	return b.String()
}

// snapshot renders the sanitized request for the DLQ record.
func (w *Worker) snapshot(m *MaterializedRequest) []byte {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return raw
}

// fencedFrom rebuilds the fencing identity carried by a dispatch message; it
// matches the FencedExecution the original Claim returned. The lease owner
// MUST come from the message (the claim stored the dispatcher's identity, not
// this worker's); the workerID parameter only backstops a hand-built message
// that predates the field, so stale messages keep failing closed on the real
// transport instead of impersonating a fresh claim.
func fencedFrom(msg PublishMessage, workerID string) *FencedExecution {
	owner := msg.LeaseOwner
	if owner == "" {
		owner = workerID
	}
	return &FencedExecution{
		ExecutionID:     msg.ExecutionID,
		WorkspaceID:     msg.WorkspaceID,
		LeaseOwner:      owner,
		LeaseGeneration: msg.Generation,
		LeaseExpiresAt:  msg.LeaseExpiresAt,
		Attempt:         msg.Attempt,
		MaxAttempts:     msg.MaxAttempts,
		RetryID:         msg.RetryID,
		AIEmployeeID:    msg.AIEmployeeID,
	}
}
