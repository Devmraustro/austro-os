package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// dispStore is a scripted store exercising the dispatcher surface; the
// embedded nil ExecutionStore supplies the untouched methods.
type dispStore struct {
	ExecutionStore
	staleIDs       []uuid.UUID
	dueIDs         []uuid.UUID
	reclaimed      map[uuid.UUID]*FencedExecution
	claimed        map[uuid.UUID]*FencedExecution
	reclaimErr     map[uuid.UUID]error
	claimErr       map[uuid.UUID]error
	rescheduled    []*FencedExecution
	rescheduleSeqs []int
	deadLetter     []*FencedExecution
	dispatched     []recordedDispatch
	ws             uuid.UUID
}

type recordedDispatch struct {
	retryID  uuid.UUID
	workerID string
	gen      int64
	success  bool
}

func (d *dispStore) GetStale(ctx context.Context, ws uuid.UUID, limit int) ([]uuid.UUID, error) {
	return d.staleIDs, nil
}

func (d *dispStore) ReclaimStale(ctx context.Context, ws, id uuid.UUID, workerID string, leaseTTL time.Duration) (*FencedExecution, error) {
	if err := d.reclaimErr[id]; err != nil {
		return nil, err
	}
	if f, ok := d.reclaimed[id]; ok {
		d.reclaimed[id] = nil
		d.staleIDs = removeID(d.staleIDs, id)
		return f, nil
	}
	return nil, ErrNotFound
}

func (d *dispStore) GetDue(ctx context.Context, ws uuid.UUID, limit int) ([]uuid.UUID, error) {
	return d.dueIDs, nil
}

func (d *dispStore) Claim(ctx context.Context, ws, id uuid.UUID, workerID string, leaseTTL time.Duration) (*FencedExecution, error) {
	if err := d.claimErr[id]; err != nil {
		return nil, err
	}
	if f, ok := d.claimed[id]; ok {
		d.claimed[id] = nil
		return f, nil
	}
	return nil, ErrNotFound
}

func removeID(ids []uuid.UUID, id uuid.UUID) []uuid.UUID {
	out := ids[:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

func (d *dispStore) Reschedule(ctx context.Context, f *FencedExecution, next time.Time, seq int) error {
	d.rescheduled = append(d.rescheduled, f)
	d.rescheduleSeqs = append(d.rescheduleSeqs, seq)
	return nil
}

func (d *dispStore) FailAndDLQ(ctx context.Context, f *FencedExecution, code ErrorCode, detail string, snapshot []byte) error {
	d.deadLetter = append(d.deadLetter, f)
	return nil
}

func (d *dispStore) RecordDispatch(ctx context.Context, ws, retryID uuid.UUID, workerID string, gen int64, success bool) error {
	d.dispatched = append(d.dispatched, recordedDispatch{retryID, workerID, gen, success})
	return nil
}

type recorder struct {
	err  error
	msgs []PublishMessage
}

func (r *recorder) Publish(ctx context.Context, msg PublishMessage) error {
	r.msgs = append(r.msgs, msg)
	return r.err
}

func fenced(id uuid.UUID, gen int64, attempt, max int) *FencedExecution {
	return &FencedExecution{
		ExecutionID:     id,
		WorkspaceID:     uuidMust(1),
		LeaseOwner:      "worker-test",
		LeaseGeneration: gen,
		LeaseExpiresAt:  time.Now().Add(time.Minute),
		Attempt:         attempt,
		MaxAttempts:     max,
		RetryID:         uuidMust(42),
		AIEmployeeID:    uuidMust(2),
	}
}

func newTestDispatcher(store *dispStore, pub Publisher) *Dispatcher {
	d := NewDispatcher(store, pub, "worker-test", 30*time.Second, time.Second, time.Minute)
	d.now = func() time.Time { return time.Unix(1_000, 0) }
	return d
}

func TestSweepDispatchesDueAndRecordsSuccess(t *testing.T) {
	first, second := uuidMust(10), uuidMust(11)
	store := &dispStore{
		dueIDs: []uuid.UUID{first, second},
		claimed: map[uuid.UUID]*FencedExecution{
			first: fenced(first, 3, 2, 4),
		},
		ws: uuidMust(1),
	}
	pub := &recorder{}
	d := newTestDispatcher(store, pub)
	if err := d.Sweep(context.Background(), store.ws); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(pub.msgs) != 1 {
		t.Fatalf("published %d, want 1", len(pub.msgs))
	}
	p := pub.msgs[0]
	// The publish must carry the claim owner: without it the worker (a distinct
	// process) can never reproduce the stored lease_owner and every fenced
	// write fails as stale.
	if p.ExecutionID != first || p.RetryID != uuidMust(42) || p.Generation != 3 {
		t.Fatalf("published wrong message: %+v", p)
	}
	if p.LeaseOwner != "worker-test" {
		t.Fatalf("published message must carry the claim owner, got %q", p.LeaseOwner)
	}
	if len(store.dispatched) != 1 || !store.dispatched[0].success ||
		store.dispatched[0].retryID != uuidMust(42) || store.dispatched[0].gen != 3 {
		t.Fatalf("dispatch not recorded as success: %+v", store.dispatched)
	}
}

func TestSweepRecordsFailedPublishWithoutReschedule(t *testing.T) {
	id := uuidMust(10)
	store := &dispStore{
		dueIDs:  []uuid.UUID{id},
		claimed: map[uuid.UUID]*FencedExecution{id: fenced(id, 1, 1, 3)},
		ws:      uuidMust(1),
	}
	pub := &recorder{err: errors.New("broker down")}
	d := newTestDispatcher(store, pub)
	if err := d.Sweep(context.Background(), store.ws); err != nil {
		t.Fatalf("publish failure must not fail the sweep: %v", err)
	}
	if len(store.dispatched) != 1 || store.dispatched[0].success {
		t.Fatalf("expected one failed dispatch record, got %+v", store.dispatched)
	}
	if len(store.rescheduled) != 0 {
		t.Fatal("a failed publish must leave the retry claimed for recovery, not reschedule")
	}
}

func TestSweepSkipsClaimRace(t *testing.T) {
	id := uuidMust(10)
	store := &dispStore{
		dueIDs: []uuid.UUID{id}, // Claim will return ErrNotFound: another dispatcher won
		ws:     uuidMust(1),
	}
	d := newTestDispatcher(store, &recorder{})
	if err := d.Sweep(context.Background(), store.ws); err != nil {
		t.Fatalf("lost claim race must not fail the sweep: %v", err)
	}
	if len(store.dispatched) != 0 {
		t.Fatal("a lost claim must not be published")
	}
}

func TestRecoverStaleReschedulesRetryableCrash(t *testing.T) {
	id := uuidMust(10)
	store := &dispStore{
		ws:       uuidMust(1),
		staleIDs: []uuid.UUID{id},
		reclaimed: map[uuid.UUID]*FencedExecution{
			id: fenced(id, 5, 2, 4), // no LastErrorCode/Retryable: crashed
		},
	}
	d := newTestDispatcher(store, &recorder{})
	if err := d.Sweep(context.Background(), store.ws); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.rescheduled) != 1 || len(store.deadLetter) != 0 {
		t.Fatalf("want 1 reschedule, got rescheduled=%d dlq=%d", len(store.rescheduled), len(store.deadLetter))
	}
	// crash -> worker_lease_expired -> retryable, attempts remain -> next retry
	if store.rescheduled[0].ExecutionID != id {
		t.Fatalf("rescheduled wrong execution: %v", store.rescheduled[0].ExecutionID)
	}
}

func TestRecoverStaleDLQnonRetryable(t *testing.T) {
	id := uuidMust(10)
	code := CodeCanceled
	f := fenced(id, 2, 1, 3)
	f.LastErrorCode = &code
	no := false
	f.Retryable = &no
	store := &dispStore{
		ws:        uuidMust(1),
		staleIDs:  []uuid.UUID{id},
		reclaimed: map[uuid.UUID]*FencedExecution{id: f},
	}
	d := newTestDispatcher(store, &recorder{})
	if err := d.Sweep(context.Background(), store.ws); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.deadLetter) != 1 || len(store.rescheduled) != 0 {
		t.Fatalf("want DLQ only, got dlq=%d rescheduled=%d", len(store.deadLetter), len(store.rescheduled))
	}
}

func TestRecoverStaleDLQExhaustedAttempts(t *testing.T) {
	id := uuidMust(10)
	f := fenced(id, 4, 3, 3) // retryable by default, but attempt == max_attempts
	store := &dispStore{
		ws:        uuidMust(1),
		staleIDs:  []uuid.UUID{id},
		reclaimed: map[uuid.UUID]*FencedExecution{id: f},
	}
	d := newTestDispatcher(store, &recorder{})
	if err := d.Sweep(context.Background(), store.ws); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.deadLetter) != 1 {
		t.Fatalf("attempts exhausted must DLQ, got %d", len(store.deadLetter))
	}
}

func TestRecoverStaleReschedulesCrashWithNoOutstandingRetry(t *testing.T) {
	id := uuidMust(10)
	// ReclaimStale resolves the outstanding retry with a subquery over
	// state='pending'. A crash that happens between the claim and the retry
	// becoming pending (or after it was already dispatched) therefore yields a
	// NULL retry id. Recovery must be identical in that case, because the
	// reschedule is keyed on the execution and the lease generation, never on
	// the retry id: the execution must be rescheduled, not dead-lettered, and
	// the next retry sequence must advance.
	f := fenced(id, 7, 1, 3)
	f.RetryID = uuid.Nil
	store := &dispStore{
		ws:        uuidMust(1),
		staleIDs:  []uuid.UUID{id},
		reclaimed: map[uuid.UUID]*FencedExecution{id: f},
	}
	d := newTestDispatcher(store, &recorder{})
	if err := d.Sweep(context.Background(), store.ws); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(store.rescheduled) != 1 || len(store.deadLetter) != 0 {
		t.Fatalf("a crash without an outstanding retry must be rescheduled, not dead-lettered: rescheduled=%d dlq=%d",
			len(store.rescheduled), len(store.deadLetter))
	}
	gotSeq := store.rescheduleSeqs[0]
	if gotSeq != f.Attempt+1 {
		t.Fatalf("reschedule sequence = %d, want %d (attempt must advance)", gotSeq, f.Attempt+1)
	}
}

func TestRecoverStaleSkipsReclaimRace(t *testing.T) {
	id := uuidMust(10)
	store := &dispStore{
		ws:         uuidMust(1),
		staleIDs:   []uuid.UUID{id},
		reclaimErr: map[uuid.UUID]error{id: ErrNotFound},
		reclaimed:  map[uuid.UUID]*FencedExecution{},
	}
	d := newTestDispatcher(store, &recorder{})
	if err := d.Sweep(context.Background(), store.ws); err != nil {
		t.Fatalf("lost reclaim must not fail the sweep: %v", err)
	}
	if len(store.rescheduled) != 0 && len(store.deadLetter) != 0 {
		t.Fatal("a lost reclaim must not move the execution")
	}
}

func TestRetryDelaySequence(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{6, 32 * time.Second},
		{7, time.Minute}, // 64s exceeds the 60s cap
	}
	for _, c := range cases {
		if got := RetryDelay(c.attempt, time.Second, time.Minute); got != c.want {
			t.Fatalf("RetryDelay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
	if got := RetryDelay(8, time.Second, time.Minute); got != time.Minute {
		t.Fatalf("cap not applied: %v", got)
	}
	if got := RetryDelay(0, time.Second, time.Minute); got != time.Second {
		t.Fatalf("attempt<1 must fall back to base: %v", got)
	}
}

func TestAttemptWindowShrinksLinearly(t *testing.T) {
	full := 30 * time.Second
	var prev time.Duration
	for attempt := 1; attempt <= 3; attempt++ {
		w := AttemptWindow(full, attempt, 3)
		if w <= 0 || w > full {
			t.Fatalf("window %d out of range: %v", attempt, w)
		}
		if attempt > 1 && w >= prev {
			t.Fatalf("windows must shrink: attempt %d got %v after %v", attempt, w, prev)
		}
		prev = w
	}
	if w := AttemptWindow(full, 1, 1); w != full {
		t.Fatalf("single attempt must keep the whole budget: %v", w)
	}
}
