package execution

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
)

// Dispatcher claims due work and stale leases, publishes claimed retries and
// records the dispatch outcome. It is the only consumer of Claim/ReclaimStale
// and the only producer of publish side effects, so at-least-once delivery is
// bounded by exactly one broker touch per claimed retry. Redundant publishes
// are harmless because RecordDispatch is fenced on (retry, worker, generation).
//
// All store methods are workspace-scoped; Sweep drives one workspace per call,
// which keeps concurrency control in the database (advisory locks per
// employee) rather than in the dispatcher process.
type Dispatcher struct {
	store      ExecutionStore
	pub        Publisher
	log        *slog.Logger
	workerID   string
	leaseTTL   time.Duration
	baseDelay  time.Duration
	maxDelay   time.Duration
	sweepLimit int
	now        func() time.Time
}

// NewDispatcher wires a dispatcher to its store and publisher. baseDelay and
// maxDelay bound the retry backoff; leaseTTL is the lease granted on each
// claim/reclaim. workerID is the fencing identity (NewWorkerID by default).
func NewDispatcher(store ExecutionStore, pub Publisher, workerID string, leaseTTL, baseDelay, maxDelay time.Duration) *Dispatcher {
	return &Dispatcher{
		store:      store,
		pub:        pub,
		log:        slog.Default(),
		workerID:   workerID,
		leaseTTL:   leaseTTL,
		baseDelay:  baseDelay,
		maxDelay:   maxDelay,
		sweepLimit: 100,
		now:        time.Now,
	}
}

// NewWorkerID builds the fencing identity for a dispatcher or worker process:
// host-pid, which is unique enough across the deployment for lease ownership.
func NewWorkerID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// Run sweeps a single workspace on a fixed interval until the context ends.
// Composition starts one Run per workspace (see the wiring phase).
func (d *Dispatcher) Run(ctx context.Context, workspaceID uuid.UUID, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	if err := d.Sweep(ctx, workspaceID); err != nil {
		d.log.Error("dispatch sweep", "workspace", workspaceID, "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.Sweep(ctx, workspaceID); err != nil {
				d.log.Error("dispatch sweep", "workspace", workspaceID, "error", err)
			}
		}
	}
}

// Sweep performs one recovery pass and one dispatch pass for a workspace.
// A failed store call surfaces its error; a failed publish is recorded as a
// failed dispatch and the retry is left claimed for the lease to expire, after
// which recovery re-dispatches it (at-least-once, fenced).
func (d *Dispatcher) Sweep(ctx context.Context, workspaceID uuid.UUID) error {
	if err := d.recoverStale(ctx, workspaceID); err != nil {
		return err
	}
	return d.dispatchDue(ctx, workspaceID)
}

// recoverStale reclaims expired leases and either reschedules a retryable
// failure or fails the execution onto the DLQ. Reclaim races are lost to a
// heartbeat or another recoverer by ErrNotFound, which is expected and skipped.
func (d *Dispatcher) recoverStale(ctx context.Context, workspaceID uuid.UUID) error {
	for {
		ids, err := d.store.GetStale(ctx, workspaceID, d.sweepLimit)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		reclaimed := false
		for _, id := range ids {
			f, err := d.store.ReclaimStale(ctx, workspaceID, id, d.workerID, d.leaseTTL)
			if errors.Is(err, ErrNotFound) {
				continue // someone else already renewed or reclaimed it
			}
			if err != nil {
				return err
			}
			reclaimed = true
			if err := d.resolveRecovered(ctx, f); err != nil {
				return err
			}
		}
		// Stop once nothing was reclaimed: the listed rows are being handled
		// elsewhere, and re-listing them would spin forever.
		if !reclaimed {
			return nil
		}
	}
}

// resolveRecovered classifies a reclaimed execution. A persisted classification
// decides; a crash (nil Retryable) is treated as worker_lease_expired and
// retried while attempts remain. Either way the decision is terminal-or-retry
// and is made exactly once per reclaim, because the fenced transaction moves
// the execution out of running.
func (d *Dispatcher) resolveRecovered(ctx context.Context, f *FencedExecution) error {
	code := CodeWorkerLeaseExpired
	retryable := true
	if f.LastErrorCode != nil {
		code = *f.LastErrorCode
	}
	if f.Retryable != nil {
		retryable = *f.Retryable
	}

	if !retryable || f.Attempt >= f.MaxAttempts {
		return d.store.FailAndDLQ(ctx, f, code, "lease expired after non-retryable failure", nil)
	}

	next := d.now().Add(RetryDelay(f.Attempt, d.baseDelay, d.maxDelay))
	return d.store.Reschedule(ctx, f, next, f.Attempt+1)
}

// dispatchDue claims due executions oldest-first and publishes each claim.
// Claim is the fence: a row claimed by another dispatcher between list and
// claim yields ErrNotFound and is skipped, so dispatch work never duplicates
// within the same lease.
func (d *Dispatcher) dispatchDue(ctx context.Context, workspaceID uuid.UUID) error {
	ids, err := d.store.GetDue(ctx, workspaceID, d.sweepLimit)
	if err != nil {
		return err
	}
	for _, id := range ids {
		f, err := d.store.Claim(ctx, workspaceID, id, d.workerID, d.leaseTTL)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		err = d.pub.Publish(ctx, PublishMessage{
			WorkspaceID:    f.WorkspaceID,
			ExecutionID:    f.ExecutionID,
			AIEmployeeID:   f.AIEmployeeID,
			Attempt:        f.Attempt,
			MaxAttempts:    f.MaxAttempts,
			RetryID:        f.RetryID,
			Generation:     f.LeaseGeneration,
			LeaseExpiresAt: f.LeaseExpiresAt,
			LeaseOwner:     d.workerID,
		})
		if err != nil {
			d.log.Error("publish failed, dispatch recorded failed",
				"execution", f.ExecutionID, "retry", f.RetryID,
				"worker", d.workerID, "generation", f.LeaseGeneration, "error", err)
		}
		if err := d.store.RecordDispatch(ctx, f.WorkspaceID, f.RetryID, d.workerID, f.LeaseGeneration, err == nil); err != nil {
			return err
		}
	}
	return nil
}
