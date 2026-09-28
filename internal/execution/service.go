package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// DefaultMaxAttempts is applied when an execution request does not state one.
const DefaultMaxAttempts = 3

// Service orchestrates the execution lifecycle against an ExecutionStore. It
// holds no connection state and imports no infrastructure, keeping the domain
// direction single (service -> store port, never the reverse).
type Service struct {
	store ExecutionStore
	now   func() time.Time
}

// NewService returns a Service over the given store.
func NewService(store ExecutionStore) *Service {
	return &Service{store: store, now: time.Now}
}

// Admit is the admission gate. Idempotency semantics, in precedence order:
//
//  1. keyed + identical payload + terminal(completed): 200 with the result
//  2. keyed + identical payload + pending/running:     202 replay of the same
//     execution (Created=false)
//  3. keyed + different payload (hash, employee or task): ErrIdempotencyKey
//     (the caller surfaces a 409)
//  4. capacity exhausted for the employee: ErrCapacityExceeded (429)
//  5. otherwise: a fresh pending execution is created (Created=true, 202)
func (s *Service) Admit(ctx context.Context, req AdmitRequest) (AdmitResult, error) {
	if req.WorkspaceID == uuid.Nil {
		return AdmitResult{}, ErrWorkspaceMismatch
	}
	if req.AIEmployeeID == uuid.Nil {
		return AdmitResult{}, ErrInvalidInput
	}
	if req.MaxAttempts <= 0 {
		req.MaxAttempts = DefaultMaxAttempts
	}

	hash, err := RequestHash(req)
	if err != nil {
		return AdmitResult{}, err
	}

	res, err := s.store.AdmissionAdmit(ctx, req, hash)
	if err != nil {
		return AdmitResult{}, err
	}

	if !res.Created {
		// A pre-existing execution is only a replay when the caller is asking
		// for exactly the same thing.
		if !sameRequest(req, res.Execution) || res.Execution.RequestHash != hash {
			return AdmitResult{}, ErrIdempotencyKey
		}
	}
	return res, nil
}

// Cancel aborts a runnable execution the caller owns. Cancellation is idempotent:
// cancelling an already-cancelled execution is not an error.
func (s *Service) Cancel(ctx context.Context, workspaceID, id uuid.UUID) error {
	return s.store.Cancel(ctx, workspaceID, id)
}

// Get returns one execution inside the caller's workspace.
func (s *Service) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Execution, error) {
	return s.store.Get(ctx, workspaceID, id)
}

func sameRequest(req AdmitRequest, exec *Execution) bool {
	if exec == nil {
		return false
	}
	if exec.AIEmployeeID != req.AIEmployeeID {
		return false
	}
	if (exec.TaskID == nil) != (req.TaskID == nil) {
		return false
	}
	if exec.TaskID != nil && req.TaskID != nil && *exec.TaskID != *req.TaskID {
		return false
	}
	return true
}

// RequestHash is the canonical fingerprint of an admission payload: the
// employee, the optional task and the attempt budget. The idempotency key and
// workspace deliberately do not participate — the (workspace, key, hash)
// triple is what defines a replay — so the same payload always yields the same
// hash regardless of who admitted it.
func RequestHash(req AdmitRequest) (string, error) {
	type canonical struct {
		AIEmployeeID uuid.UUID  `json:"ai_employee_id"`
		TaskID       *uuid.UUID `json:"task_id"`
		MaxAttempts  int        `json:"max_attempts"`
	}
	raw, err := json.Marshal(canonical{
		AIEmployeeID: req.AIEmployeeID,
		TaskID:       req.TaskID,
		MaxAttempts:  req.MaxAttempts,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
