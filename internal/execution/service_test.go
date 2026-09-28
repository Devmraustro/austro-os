package execution

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
)

func uuidMust(n int) uuid.UUID {
	var u uuid.UUID
	binary.BigEndian.PutUint64(u[0:8], uint64(n))
	return u
}

func uuidPtr(u uuid.UUID) *uuid.UUID { return &u }

// fakeStore implements only the surface the Service touches; the embedded nil
// ExecutionStore provides the rest of the method set untouched.
type fakeStore struct {
	ExecutionStore
	onAdmission func(ctx context.Context, req AdmitRequest, hash string) (AdmitResult, error)
	onCancel    func(ctx context.Context, workspaceID, id uuid.UUID) error
	admitted    []AdmitRequest
	cancelled   []uuid.UUID
	existing    map[string]*Execution // keyed by idempotency key
	seq         int
}

func (f *fakeStore) AdmissionAdmit(ctx context.Context, req AdmitRequest, hash string) (AdmitResult, error) {
	f.admitted = append(f.admitted, req)
	if f.onAdmission != nil {
		return f.onAdmission(ctx, req, hash)
	}
	if req.IdempotencyKey != "" {
		if ex, ok := f.existing[req.IdempotencyKey]; ok {
			return AdmitResult{Execution: ex, Created: false, Completed: ex.Status == StatusCompleted}, nil
		}
	}
	f.seq++
	var key *string
	if req.IdempotencyKey != "" {
		key = &req.IdempotencyKey
	}
	ex := &Execution{
		ID:             uuidMust(f.seq),
		WorkspaceID:    req.WorkspaceID,
		AIEmployeeID:   req.AIEmployeeID,
		TaskID:         req.TaskID,
		Status:         StatusPending,
		MaxAttempts:    req.MaxAttempts,
		IdempotencyKey: key,
		RequestHash:    hash,
	}
	if req.IdempotencyKey != "" {
		f.existing[req.IdempotencyKey] = ex
	}
	return AdmitResult{Execution: ex, Created: true}, nil
}

func (f *fakeStore) Cancel(ctx context.Context, workspaceID, id uuid.UUID) error {
	f.cancelled = append(f.cancelled, id)
	if f.onCancel != nil {
		return f.onCancel(ctx, workspaceID, id)
	}
	return nil
}

func TestAdmitDefaultsMaxAttempts(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	res, err := s.Admit(context.Background(), AdmitRequest{
		WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2),
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !res.Created {
		t.Fatal("expected a fresh execution")
	}
	if res.Execution.MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("MaxAttempts = %d, want %d", res.Execution.MaxAttempts, DefaultMaxAttempts)
	}
}

func TestAdmitRejectsNilWorkspace(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	if _, err := s.Admit(context.Background(), AdmitRequest{AIEmployeeID: uuidMust(1)}); err != ErrWorkspaceMismatch {
		t.Fatalf("got %v, want ErrWorkspaceMismatch", err)
	}
}

func TestAdmitRejectsNilEmployee(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	if _, err := s.Admit(context.Background(), AdmitRequest{WorkspaceID: uuidMust(1)}); err != ErrInvalidInput {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
}

func TestAdmitIdempotentReplaySamePayload(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	key := "k-1"
	req := func() AdmitRequest {
		return AdmitRequest{
			WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2),
			MaxAttempts: 5, IdempotencyKey: key,
			TaskID: uuidPtr(uuidMust(3)),
		}
	}

	first, err := s.Admit(context.Background(), req())
	if err != nil || !first.Created {
		t.Fatalf("first admit: created=%v err=%v", first.Created, err)
	}
	second, err := s.Admit(context.Background(), req())
	if err != nil {
		t.Fatalf("replay admit: %v", err)
	}
	if second.Created {
		t.Fatal("second admit must be a replay, not a new execution")
	}
	if second.Execution.ID != first.Execution.ID {
		t.Fatalf("replay returned different execution %v != %v", second.Execution.ID, first.Execution.ID)
	}
}

func TestAdmitReplayCompletedIsIdentified(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	req := AdmitRequest{
		WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2),
		MaxAttempts: 3, IdempotencyKey: "k-done",
	}
	first, err := s.Admit(context.Background(), req)
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	first.Execution.Status = StatusCompleted // same pointer as the stored row

	second, err := s.Admit(context.Background(), req)
	if err != nil {
		t.Fatalf("replay admit: %v", err)
	}
	if !second.Completed {
		t.Fatal("replay of a completed execution must report Completed")
	}
	if second.Created {
		t.Fatal("replay must not be created")
	}
}

func TestAdmitReplayDifferentPayloadConflicts(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	base := AdmitRequest{
		WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2),
		MaxAttempts: 3, IdempotencyKey: "k-conf",
	}
	if _, err := s.Admit(context.Background(), base); err != nil {
		t.Fatalf("first admit: %v", err)
	}

	changed := base
	changed.MaxAttempts = 9 // alters the request hash even though the key matches
	if _, err := s.Admit(context.Background(), changed); err != ErrIdempotencyKey {
		t.Fatalf("got %v, want ErrIdempotencyKey", err)
	}
}

func TestAdmitReplayDifferentEmployeeConflicts(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	base := AdmitRequest{
		WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2),
		MaxAttempts: 3, IdempotencyKey: "k-emp",
	}
	if _, err := s.Admit(context.Background(), base); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	other := base
	other.AIEmployeeID = uuidMust(99)
	if _, err := s.Admit(context.Background(), other); err != ErrIdempotencyKey {
		t.Fatalf("got %v, want ErrIdempotencyKey", err)
	}
}

func TestAdmitPropagatesCapacity(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	f.onAdmission = func(ctx context.Context, req AdmitRequest, hash string) (AdmitResult, error) {
		return AdmitResult{}, ErrCapacityExceeded
	}
	s := NewService(f)
	_, err := s.Admit(context.Background(), AdmitRequest{
		WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2), MaxAttempts: 3,
	})
	if err != ErrCapacityExceeded {
		t.Fatalf("got %v, want ErrCapacityExceeded", err)
	}
}

func TestKeylessAdmissionsAlwaysCreate(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	for i := 0; i < 2; i++ {
		res, err := s.Admit(context.Background(), AdmitRequest{
			WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2), MaxAttempts: 3,
		})
		if err != nil || !res.Created {
			t.Fatalf("keyless admit %d: created=%v err=%v", i, res.Created, err)
		}
	}
}

func TestCancelPassesThrough(t *testing.T) {
	f := &fakeStore{existing: map[string]*Execution{}}
	s := NewService(f)
	id := uuidMust(7)
	if err := s.Cancel(context.Background(), uuidMust(1), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(f.cancelled) != 1 || f.cancelled[0] != id {
		t.Fatalf("cancel not forwarded: %v", f.cancelled)
	}
}

func TestRequestHashDeterminism(t *testing.T) {
	req := AdmitRequest{WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2), MaxAttempts: 3}
	h1, err := RequestHash(req)
	if err != nil {
		t.Fatalf("RequestHash: %v", err)
	}
	h2, _ := RequestHash(AdmitRequest{WorkspaceID: uuidMust(9), AIEmployeeID: uuidMust(2), MaxAttempts: 3})
	if h1 != h2 {
		t.Fatal("workspace must not participate in the request hash")
	}
	h3, _ := RequestHash(AdmitRequest{WorkspaceID: uuidMust(1), AIEmployeeID: uuidMust(2), MaxAttempts: 4})
	if h1 == h3 {
		t.Fatal("max_attempts must participate in the request hash")
	}
}
