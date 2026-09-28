package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"austro-os/internal/ai"

	"github.com/google/uuid"
)

// workStore records the finalization decisions the worker makes.
type workStore struct {
	ExecutionStore
	heartbeatErr    error
	persisted       []PersistRecord
	completed       []*FencedExecution
	completedResult [][]byte
	rescheduled     []ReschedRecord
	deadlettered    []*FencedExecution
	dlqSnapshot     [][]byte
}

type PersistRecord struct {
	f      *FencedExecution
	code   ErrorCode
	retry  bool
	detail string
}
type ReschedRecord struct {
	f   *FencedExecution
	seq int
}

func (w *workStore) Heartbeat(ctx context.Context, f *FencedExecution, leaseTTL time.Duration) error {
	return w.heartbeatErr
}

func (w *workStore) PersistError(ctx context.Context, f *FencedExecution, code ErrorCode, retry bool, detail string) error {
	w.persisted = append(w.persisted, PersistRecord{f, code, retry, detail})
	return nil
}

func (w *workStore) Complete(ctx context.Context, f *FencedExecution, result []byte) error {
	w.completed = append(w.completed, f)
	w.completedResult = append(w.completedResult, result)
	return nil
}

func (w *workStore) Reschedule(ctx context.Context, f *FencedExecution, next time.Time, seq int) error {
	w.rescheduled = append(w.rescheduled, ReschedRecord{f, seq})
	return nil
}

func (w *workStore) FailAndDLQ(ctx context.Context, f *FencedExecution, code ErrorCode, detail string, snapshot []byte) error {
	w.deadlettered = append(w.deadlettered, f)
	w.dlqSnapshot = append(w.dlqSnapshot, snapshot)
	return nil
}

type fakeProvider struct {
	err   error
	block <-chan struct{}
}

func (p *fakeProvider) Complete(ctx context.Context, req ai.CompletionRequest) (ai.CompletionResult, error) {
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return ai.CompletionResult{}, ctx.Err()
		}
	}
	if p.err != nil {
		return ai.CompletionResult{}, p.err
	}
	if err := req.Scope.WorkspaceID == ""; err != false {
		return ai.CompletionResult{}, ai.ErrWorkspaceRequired
	}
	return ai.CompletionResult{Text: "resolved"}, nil
}

func (p *fakeProvider) Embed(ctx context.Context, req ai.EmbedRequest) (ai.EmbeddingResult, error) {
	return ai.EmbeddingResult{}, ai.ErrInvalidInput
}

func (p *fakeProvider) Classify(ctx context.Context, req ai.ClassificationRequest) (ai.ClassificationResult, error) {
	return ai.ClassificationResult{}, ai.ErrInvalidInput
}

type fakeMat struct {
	m   *MaterializedRequest
	err error
}

func (m *fakeMat) Materialize(ctx context.Context, ws, id uuid.UUID) (*MaterializedRequest, error) {
	return m.m, m.err
}

func workerArgs() (PublishMessage, *Worker, *workStore, *fakeProvider) {
	store := &workStore{}
	msg := PublishMessage{
		WorkspaceID:    uuidMust(1),
		ExecutionID:    uuidMust(10),
		AIEmployeeID:   uuidMust(2),
		Attempt:        1,
		MaxAttempts:    3,
		RetryID:        uuidMust(11),
		Generation:     2,
		LeaseExpiresAt: time.Now().Add(time.Minute),
	}
	prov := &fakeProvider{}
	w := NewWorker(store, &fakeMat{m: &MaterializedRequest{
		TaskTitle: "Write the summary", AIEmployeeName: "Ada", AIEmployeeRole: "Analyst", Capabilities: []string{"writing"},
	}}, prov, "worker-test", 30*time.Second, 15*time.Second, 25*time.Second, time.Second, time.Minute)
	return msg, w, store, prov
}

func TestWorkerCompletesResult(t *testing.T) {
	msg, w, store, _ := workerArgs()
	out, err := w.Run(context.Background(), msg)
	if err != nil || out != OutcomeCompleted {
		t.Fatalf("outcome=%q err=%v, want completed", out, err)
	}
	if len(store.completed) != 1 || store.completed[0].ExecutionID != msg.ExecutionID {
		t.Fatalf("complete not recorded fenced: %+v", store.completed)
	}
	if len(store.persisted) != 0 || len(store.deadlettered) != 0 || len(store.rescheduled) != 0 {
		t.Fatal("completion must not also persist errors or schedule retries")
	}
}

func TestWorkerSchedulesRetryableFailure(t *testing.T) {
	msg, w, store, prov := workerArgs()
	prov.err = context.DeadlineExceeded
	out, err := w.Run(context.Background(), msg)
	if err != nil || out != OutcomeScheduled {
		t.Fatalf("outcome=%q err=%v, want scheduled", out, err)
	}
	if len(store.persisted) != 1 || store.persisted[0].code != CodeDeadlineExceeded || !store.persisted[0].retry {
		t.Fatalf("classification not persisted: %+v", store.persisted)
	}
	if len(store.rescheduled) != 1 || store.rescheduled[0].seq != 2 {
		t.Fatalf("expected reschedule seq 2: %+v", store.rescheduled)
	}
}

func TestWorkerDeadLettersNonRetryable(t *testing.T) {
	msg, w, store, prov := workerArgs()
	prov.err = errors.New("unclassified boom")
	out, err := w.Run(context.Background(), msg)
	if err != nil || out != OutcomeDeadLettered {
		t.Fatalf("outcome=%q err=%v, want deadlettered", out, err)
	}
	if len(store.deadlettered) != 1 {
		t.Fatalf("expected DLQ, got %+v", store.deadlettered)
	}
	if len(store.rescheduled) != 0 {
		t.Fatal("non-retryable failure must not be rescheduled")
	}
	if len(store.dlqSnapshot) != 1 || len(store.dlqSnapshot[0]) == 0 {
		t.Fatal("DLQ must carry the sanitized snapshot")
	}
}

func TestWorkerDeadLettersExhaustedAttempts(t *testing.T) {
	msg, w, store, prov := workerArgs()
	msg.Attempt, msg.MaxAttempts = 3, 3
	prov.err = context.DeadlineExceeded // retryable, but no attempts remain
	out, err := w.Run(context.Background(), msg)
	if err != nil || out != OutcomeDeadLettered {
		t.Fatalf("outcome=%q err=%v, want deadlettered", out, err)
	}
	if len(store.rescheduled) != 0 {
		t.Fatal("attempts exhausted must not schedule")
	}
	if len(store.deadlettered) != 1 {
		t.Fatal("expected DLQ")
	}
}

func TestWorkerAbandonsOnHeartbeatLoss(t *testing.T) {
	msg, w, store, prov := workerArgs()
	store.heartbeatErr = ErrStaleLease
	prov.block = make(chan struct{})
	w.heartbeatInterval = 10 * time.Millisecond

	out, err := w.Run(context.Background(), msg)
	if err != nil {
		t.Fatalf("heartbeat loss must abandon, not error: %v", err)
	}
	if out != OutcomeAbandoned {
		t.Fatalf("outcome=%q, want abandoned", out)
	}
	if len(store.completed) != 0 || len(store.persisted) != 0 ||
		len(store.rescheduled) != 0 || len(store.deadlettered) != 0 {
		t.Fatal("a worker that lost ownership must write nothing")
	}
}

func TestWorkerRejectsInvalidMessage(t *testing.T) {
	msg, w, _, _ := workerArgs()
	msg.ExecutionID = uuid.Nil
	if out, err := w.Run(context.Background(), msg); out != OutcomeDeadLettered || err != ErrInvalidInput {
		t.Fatalf("outcome=%q err=%v", out, err)
	}
}

func TestWorkerMaterializeFailureAbandons(t *testing.T) {
	msg, w, _, _ := workerArgs()
	w.mat = &fakeMat{err: ErrNotFound}
	if out, err := w.Run(context.Background(), msg); out != OutcomeAbandoned {
		t.Fatalf("outcome=%q err=%v, want abandoned", out, err)
	}
}

// TestWorkerFencesOnMessageLeaseOwner reproduces the two-process contract: the
// dispatcher process claims with ITS identity and the worker process must fence
// every write against that stored owner, not its own. Before LeaseOwner existed
// on the message the worker rebuilt the fence with its own pid, which never
// matches the row and turns every real-transport run into an abandoned attempt
// (and, for a fast provider, a requeue poison loop).
func TestWorkerFencesOnMessageLeaseOwner(t *testing.T) {
	msg, w, store, _ := workerArgs()
	msg.LeaseOwner = "api-host-1000" // the dispatcher process's claim owner
	w.workerID = "worker-host-2000"  // a different process, as in production

	if out, err := w.Run(context.Background(), msg); err != nil || out != OutcomeCompleted {
		t.Fatalf("outcome=%q err=%v, want completed", out, err)
	}
	if len(store.completed) != 1 {
		t.Fatalf("expected a completed fence, got %+v", store.completed)
	}
	if got := store.completed[0].LeaseOwner; got != msg.LeaseOwner {
		t.Fatalf("worker fenced with its own identity %q, want the claim owner %q", got, msg.LeaseOwner)
	}
}
