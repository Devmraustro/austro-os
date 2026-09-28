package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"time"

	"austro-os/internal/execution"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
)

// ExecutionStore is the concrete Postgres adapter for execution.ExecutionStore.
//
// Every method opens a transaction and binds app.current_workspace on that
// exact connection (transaction-local scope), so row-level security genuinely
// constrains the operation even for a caller whose connection could otherwise
// see more. Explicit workspace_id guards are kept as defense-in-depth.
//
// The lease methods implement the frozen runtime invariants at the SQL layer:
//   - Claim is the only statement that ever increments attempt and assigns a
//     lease, and it does both to the execution and its outstanding retry row in
//     one atomic CTE, so a partial claim cannot exist.
//   - Every later mutation re-asserts status='running' AND lease_owner=$worker
//     AND lease_generation=$generation in its WHERE clause and fails with
//     ErrStaleLease when RowsAffected is 0, so a stale worker can never write.
//   - Terminal or reschedule transitions clear the lease fingerprint; a
//     processed retry row always has worker_id NULL.
type ExecutionStore struct {
	db *sql.DB
}

// NewExecutionStore returns a Postgres-backed implementation of
// execution.ExecutionStore.
func NewExecutionStore(db *sql.DB) *ExecutionStore {
	return &ExecutionStore{db: db}
}

func (s *ExecutionStore) beginTx(ctx context.Context, workspaceID uuid.UUID) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('app.current_workspace', $1, true)",
		workspaceID.String()); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// isUniqueViolation reports whether err is a Postgres unique (or exclusion)
// constraint violation, which is how a duplicate idempotency key surfaces.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// AdmissionAdmit is the canonical admission transaction: SERIALIZABLE so the
// active-count read and the insert cannot interleave, an advisory transaction
// lock per (workspace, employee) so concurrent admissions to the same employee
// serialize, an idempotency resolution, and a capacity check before insert.
func (s *ExecutionStore) AdmissionAdmit(ctx context.Context, req execution.AdmitRequest, requestHash string) (execution.AdmitResult, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return execution.AdmitResult{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('app.current_workspace', $1, true)",
		req.WorkspaceID.String()); err != nil {
		return execution.AdmitResult{}, err
	}

	if _, err := tx.ExecContext(ctx,
		"SELECT pg_advisory_xact_lock($1)",
		int64(admissionLockKey(req.WorkspaceID, req.AIEmployeeID))); err != nil {
		return execution.AdmitResult{}, err
	}

	// Idempotency resolution runs before capacity so a caller re-sending a
	// keyed request is not refused for a quota it has not consumed anew.
	if req.IdempotencyKey != "" {
		existing, err := scanExecution(tx.QueryRowContext(ctx,
			executionSelect+` WHERE e.workspace_id = $1 AND e.idempotency_key = $2`,
			req.WorkspaceID, req.IdempotencyKey))
		if err != nil && !errors.Is(err, execution.ErrNotFound) {
			return execution.AdmitResult{}, err
		}
		if existing != nil {
			if err := tx.Commit(); err != nil {
				return execution.AdmitResult{}, err
			}
			return execution.AdmitResult{
				Execution: existing,
				Created:   false,
				Completed: existing.Status == execution.StatusCompleted,
			}, nil
		}
	}

	var cap int
	err = tx.QueryRowContext(ctx,
		`SELECT max_concurrent_executions FROM ai_employees WHERE id = $1`, req.AIEmployeeID).Scan(&cap)
	if errors.Is(err, sql.ErrNoRows) {
		return execution.AdmitResult{}, execution.ErrNotFound
	}
	if err != nil {
		return execution.AdmitResult{}, err
	}

	var active int
	err = tx.QueryRowContext(ctx, `
		SELECT count(*) FROM ai_executions
		WHERE ai_employee_id = $1 AND status IN ('pending', 'running')`, req.AIEmployeeID).Scan(&active)
	if err != nil {
		return execution.AdmitResult{}, err
	}
	if active >= cap {
		return execution.AdmitResult{}, execution.ErrCapacityExceeded
	}

	exec := execution.Execution{
		ID:           uuid.New(),
		WorkspaceID:  req.WorkspaceID,
		AIEmployeeID: req.AIEmployeeID,
		TaskID:       req.TaskID,
		Status:       execution.StatusPending,
		MaxAttempts:  req.MaxAttempts,
		RequestHash:  requestHash,
	}
	if req.IdempotencyKey != "" {
		exec.IdempotencyKey = &req.IdempotencyKey
	}
	var id uuid.UUID
	err = tx.QueryRowContext(ctx, `
		INSERT INTO ai_executions (id, workspace_id, ai_employee_id, task_id,
		                           status, attempt, max_attempts, idempotency_key,
		                           request_hash, lease_owner, lease_generation,
		                           lease_expires_at, next_retry_at)
		VALUES ($1, $2, $3, $4, 'pending', 0, $5, $6, $7, NULL, 0, NULL, NULL)
		RETURNING id`,
		exec.ID, exec.WorkspaceID, exec.AIEmployeeID, nullableUUID(exec.TaskID),
		exec.MaxAttempts, nullableString(derefString(exec.IdempotencyKey)), exec.RequestHash).Scan(&id)
	if isUniqueViolation(err) {
		// A concurrent identical insert won the race; the advisory lock makes
		// this impossible in practice, but the database is the final arbiter.
		return execution.AdmitResult{}, execution.ErrIdempotencyKey
	}
	if err != nil {
		return execution.AdmitResult{}, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ai_execution_retries (id, execution_id, workspace_id, retry_seq,
		                                  state, generation, next_retry_at)
		VALUES ($1, $2, $3, 1, 'pending', 0, NULL)`,
		uuid.New(), exec.ID, exec.WorkspaceID); err != nil {
		return execution.AdmitResult{}, err
	}
	// The sanitized context snapshot lands in the same transaction as the
	// execution so a persisted row always has its context, and the RLS policy
	// on its execution_id back-reference confines it to the executing workspace.
	if req.Context != nil {
		caps := req.Context.Capabilities
		if caps == nil {
			caps = []string{}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO execution_contexts (execution_id, task_title, task_description,
			                                ai_employee_name, ai_employee_role,
			                                team_name, department_name, capabilities)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			exec.ID, nullableString(req.Context.TaskTitle), nullableString(req.Context.TaskDescription),
			req.Context.AIEmployeeName, req.Context.AIEmployeeRole,
			nullableString(req.Context.TeamName), nullableString(req.Context.DepartmentName),
			pq.Array(caps)); err != nil {
			return execution.AdmitResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return execution.AdmitResult{}, err
	}
	exec.ID = id
	exec.CreatedAt = time.Now().UTC()
	exec.UpdatedAt = exec.CreatedAt
	return execution.AdmitResult{Execution: &exec, Created: true}, nil
}

// Cancel aborts a pending/running execution under workspace RLS. Any
// outstanding retry rows are finalized to processed/cancelled with the worker
// id cleared, which keeps the partial uniqueness index satisfied.
func (s *ExecutionStore) Cancel(ctx context.Context, workspaceID, id uuid.UUID) error {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cancelledAt time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE ai_executions
		SET status = 'cancelled',
		    last_error_code = NULL,
		    retryable = NULL,
		    error_detail = NULL,
		    lease_owner = NULL,
		    lease_expires_at = NULL,
		    cancelled_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND status IN ('pending', 'running')
		RETURNING cancelled_at`, id, workspaceID).Scan(&cancelledAt)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if e := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM ai_executions WHERE id = $1 AND workspace_id = $2)`,
			id, workspaceID).Scan(&exists); e != nil {
			return e
		}
		if exists {
			return tx.Commit() // already terminal: idempotent cancel
		}
		return execution.ErrNotFound
	}
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE ai_execution_retries
		SET state = 'processed', outcome = 'cancelled', worker_id = NULL,
		    completed_at = NOW()
		WHERE execution_id = $1 AND workspace_id = $2
		  AND state IN ('pending', 'claimed', 'dispatched')`, id, workspaceID); err != nil {
		return err
	}
	return tx.Commit()
}

// admissionLockKey derives the deterministic advisory-lock key for a
// (workspace, employee) pair: the first 8 bytes of the SHA-256 of the two ids.
// The same key must be produced by every process, so it cannot depend on
// string formatting or ordering.
func admissionLockKey(workspaceID, employeeID uuid.UUID) uint64 {
	h := sha256.Sum256(append([]byte(workspaceID.String()), []byte(employeeID.String())...))
	return binary.BigEndian.Uint64(h[:8])
}

func (s *ExecutionStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*execution.Execution, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	exec, err := scanExecution(tx.QueryRowContext(ctx, executionSelect+` WHERE e.id = $1 AND e.workspace_id = $2`, id, workspaceID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return exec, nil
}

func (s *ExecutionStore) GetByIdempotencyKey(ctx context.Context, workspaceID uuid.UUID, key string) (*execution.Execution, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	exec, err := scanExecution(tx.QueryRowContext(ctx, executionSelect+` WHERE e.workspace_id = $1 AND e.idempotency_key = $2`, workspaceID, key))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return exec, nil
}

func (s *ExecutionStore) Capacity(ctx context.Context, workspaceID, aiEmployeeID uuid.UUID) (int, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var cap int
	err = tx.QueryRowContext(ctx,
		`SELECT max_concurrent_executions FROM ai_employees WHERE id = $1`, aiEmployeeID).Scan(&cap)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, execution.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return cap, nil
}

func (s *ExecutionStore) ActiveCount(ctx context.Context, workspaceID, aiEmployeeID uuid.UUID) (int, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var n int
	err = tx.QueryRowContext(ctx, `
		SELECT count(*) FROM ai_executions
		WHERE ai_employee_id = $1 AND status IN ('pending', 'running')`, aiEmployeeID).Scan(&n)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *ExecutionStore) GetDue(ctx context.Context, workspaceID uuid.UUID, limit int) ([]uuid.UUID, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM ai_executions
		WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		ORDER BY created_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *ExecutionStore) GetStale(ctx context.Context, workspaceID uuid.UUID, limit int) ([]uuid.UUID, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM ai_executions
		WHERE status = 'running' AND lease_expires_at < NOW()
		ORDER BY lease_expires_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// Claim implements the frozen single-CAS claim. The execution update and the
// outstanding-retry update are one statement, so generation lockstep between
// the execution lease and the retry row is guaranteed by the database: the
// retry's generation is ASSIGNED from the execution's returned generation, not
// incremented independently. sql.ErrNoRows means the row was not pending or
// not yet due, which must never be mistaken for a successful claim.
func (s *ExecutionStore) Claim(ctx context.Context, workspaceID, id uuid.UUID, workerID string, leaseTTL time.Duration) (*execution.FencedExecution, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	f, err := scanFenced(tx.QueryRowContext(ctx, `
		WITH exec_claim AS (
			UPDATE ai_executions
			SET status = 'running',
			    attempt = attempt + 1,
			    lease_owner = $3,
			    lease_generation = lease_generation + 1,
			    lease_expires_at = NOW() + make_interval(secs => $4)
			WHERE id = $1 AND workspace_id = $2 AND status = 'pending'
			  AND attempt < max_attempts
			  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
			RETURNING id, workspace_id, ai_employee_id, lease_owner, lease_generation,
			          lease_expires_at, attempt, max_attempts
		),
		retry_claim AS (
			UPDATE ai_execution_retries r
			SET state = 'claimed',
			    worker_id = e.lease_owner,
			    generation = e.lease_generation,
			    claimed_at = NOW()
			FROM exec_claim e
			WHERE r.execution_id = e.id AND r.workspace_id = $2 AND r.state = 'pending'
			RETURNING r.id AS retry_id
		)
		SELECT e.id, e.workspace_id, e.ai_employee_id, e.lease_owner, e.lease_generation,
		       e.lease_expires_at, e.attempt, e.max_attempts, rc.retry_id
		FROM exec_claim e JOIN retry_claim rc ON true`,
		id, workspaceID, workerID, int(leaseTTL.Seconds())))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return f, nil
}

// ReclaimStale takes over a running execution whose lease expired. It bumps
// the generation exactly once and returns the prior classification: a NULL
// last_error_code means the prior worker crashed before persisting one, which
// recovery classifies as worker_lease_expired.
func (s *ExecutionStore) ReclaimStale(ctx context.Context, workspaceID, id uuid.UUID, workerID string, leaseTTL time.Duration) (*execution.FencedExecution, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	f, err := scanFencedWithClassification(tx.QueryRowContext(ctx, `
		UPDATE ai_executions
		SET lease_owner = $3,
		    lease_generation = lease_generation + 1,
		    lease_expires_at = NOW() + make_interval(secs => $4)
		WHERE id = $1 AND workspace_id = $2 AND status = 'running' AND lease_expires_at < NOW()
		RETURNING id, workspace_id, ai_employee_id, lease_owner, lease_generation,
		          lease_expires_at, attempt, max_attempts,
		          (SELECT r.id FROM ai_execution_retries r
		           WHERE r.execution_id = ai_executions.id
		             AND r.workspace_id = ai_executions.workspace_id
		             AND r.state = 'pending' LIMIT 1),
		          last_error_code, retryable, next_retry_at`,
		id, workspaceID, workerID, int(leaseTTL.Seconds())))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return f, nil
}

// Heartbeat renews ONLY the lease expiry. The WHERE clause re-asserts
// status/owner/generation, so a worker whose row moved on is answered with
// ErrStaleLease and never touches downstream state.
func (s *ExecutionStore) Heartbeat(ctx context.Context, f *execution.FencedExecution, leaseTTL time.Duration) error {
	tx, err := s.beginTx(ctx, f.WorkspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE ai_executions
		SET lease_expires_at = NOW() + make_interval(secs => $3)
		WHERE id = $1 AND workspace_id = $2 AND status = 'running'
		  AND lease_owner = $4 AND lease_generation = $5`,
		f.ExecutionID, f.WorkspaceID, int(leaseTTL.Seconds()), f.LeaseOwner, f.LeaseGeneration)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return execution.ErrStaleLease
	}
	return tx.Commit()
}

func (s *ExecutionStore) PersistError(ctx context.Context, f *execution.FencedExecution, code execution.ErrorCode, retryable bool, detail string) error {
	tx, err := s.beginTx(ctx, f.WorkspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE ai_executions
		SET last_error_code = $3, retryable = $4, error_detail = $5, updated_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND status = 'running'
		  AND lease_owner = $6 AND lease_generation = $7`,
		f.ExecutionID, f.WorkspaceID, int(code), retryable, nullableString(detail), f.LeaseOwner, f.LeaseGeneration)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return execution.ErrStaleLease
	}
	return tx.Commit()
}

// Complete marks the run successful, resolves the outstanding retry as
// processed/success with the worker fingerprint cleared, and releases the
// lease — one fenced transaction.
func (s *ExecutionStore) Complete(ctx context.Context, f *execution.FencedExecution, result []byte) error {
	tx, err := s.beginTx(ctx, f.WorkspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE ai_executions
		SET status = 'completed', result_json = $3::jsonb, completed_at = NOW(),
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND status = 'running'
		  AND lease_owner = $4 AND lease_generation = $5`,
		f.ExecutionID, f.WorkspaceID, string(result), f.LeaseOwner, f.LeaseGeneration)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return execution.ErrStaleLease
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE ai_execution_retries
		SET state = 'processed', outcome = 'success', worker_id = NULL, completed_at = NOW()
		WHERE execution_id = $1 AND workspace_id = $2 AND state IN ('claimed', 'dispatched')`,
		f.ExecutionID, f.WorkspaceID); err != nil {
		return err
	}
	return tx.Commit()
}

// FailAndDLQ marks the run failed, resolves the outstanding retry as
// processed/failed (worker cleared) and appends the DLQ entry — one fenced
// transaction, so exactly zero outstanding retry rows remain after commit.
func (s *ExecutionStore) FailAndDLQ(ctx context.Context, f *execution.FencedExecution, code execution.ErrorCode, detail string, snapshot []byte) error {
	tx, err := s.beginTx(ctx, f.WorkspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE ai_executions
		SET status = 'failed', last_error_code = $3, retryable = false, error_detail = $4,
		    failed_at = NOW(), lease_owner = NULL, lease_expires_at = NULL, updated_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND status = 'running'
		  AND lease_owner = $5 AND lease_generation = $6`,
		f.ExecutionID, f.WorkspaceID, int(code), nullableString(detail), f.LeaseOwner, f.LeaseGeneration)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return execution.ErrStaleLease
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE ai_execution_retries
		SET state = 'processed', outcome = 'failed', worker_id = NULL, completed_at = NOW()
		WHERE execution_id = $1 AND workspace_id = $2 AND state IN ('claimed', 'dispatched')`,
		f.ExecutionID, f.WorkspaceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ai_execution_dlq (id, workspace_id, execution_id, attempt,
		                              error_code, retryable, error_detail, contexts_snapshot)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, false, $5, $6::jsonb)`,
		f.WorkspaceID, f.ExecutionID, f.Attempt, int(code), nullableString(detail),
		nullableBytes(snapshot)); err != nil {
		return err
	}
	return tx.Commit()
}

// Reschedule returns the execution to pending with the lease cleared, resolves
// the outstanding retry as processed/failed (worker cleared) and inserts the
// next pending retry row, all in one fenced transaction. Old row finalization
// precedes the new INSERT, so the partial unique index on outstanding rows is
// always satisfiable and recovery never observes two live retries.
func (s *ExecutionStore) Reschedule(ctx context.Context, f *execution.FencedExecution, nextRetryAt time.Time, seq int) error {
	tx, err := s.beginTx(ctx, f.WorkspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE ai_executions
		SET status = 'pending', lease_owner = NULL, lease_expires_at = NULL,
		    next_retry_at = $3, updated_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND status = 'running'
		  AND lease_owner = $4 AND lease_generation = $5`,
		f.ExecutionID, f.WorkspaceID, nextRetryAt, f.LeaseOwner, f.LeaseGeneration)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return execution.ErrStaleLease
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE ai_execution_retries
		SET state = 'processed', outcome = 'failed', worker_id = NULL, completed_at = NOW()
		WHERE execution_id = $1 AND workspace_id = $2 AND state IN ('claimed', 'dispatched')`,
		f.ExecutionID, f.WorkspaceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ai_execution_retries (id, execution_id, workspace_id, retry_seq,
		                                  state, generation, next_retry_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 'pending', $4, $5)`,
		f.ExecutionID, f.WorkspaceID, seq, f.LeaseGeneration, nextRetryAt); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordDispatch appends the publish outcome to retry_dispatch_log only while
// the retry row is still outstanding for the same worker and generation, and
// flips the retry to dispatched on publish success. A stale dispatch writes
// nothing and is not an error: at-least-once delivery plus fencing make any
// number of redundant publishes safe.
func (s *ExecutionStore) RecordDispatch(ctx context.Context, workspaceID, retryID uuid.UUID, workerID string, generation int64, success bool) error {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if success {
		if _, err := tx.ExecContext(ctx, `
			UPDATE ai_execution_retries
			SET state = 'dispatched', dispatched_at = NOW()
			WHERE id = $1 AND workspace_id = $2 AND state = 'claimed'
			  AND worker_id = $3 AND generation = $4`,
			retryID, workspaceID, workerID, generation); err != nil {
			return err
		}
	}
	outcome := "failed"
	if success {
		outcome = "success"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO retry_dispatch_log (id, workspace_id, retry_id, worker_id, generation, outcome)
		SELECT gen_random_uuid(), $2, $1, $3, $4, $5
		WHERE EXISTS (
			SELECT 1 FROM ai_execution_retries r
			WHERE r.id = $1 AND r.workspace_id = $2
			  AND r.worker_id = $3 AND r.generation = $4
			  AND r.state IN ('claimed', 'dispatched'))`,
		retryID, workspaceID, workerID, generation, outcome); err != nil {
		return err
	}
	return tx.Commit()
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Materialize reconstructs the sanitized execution request for a worker. The
// admission snapshot lives in execution_contexts; a row is missing when the
// request was admitted without context, which the worker maps to ErrNotFound.
func (s *ExecutionStore) Materialize(ctx context.Context, workspaceID, executionID uuid.UUID) (*execution.MaterializedRequest, error) {
	tx, err := s.beginTx(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		m           execution.MaterializedRequest
		caps        []string
		title, desc sql.NullString
		team, dept  sql.NullString
	)
	err = tx.QueryRowContext(ctx, `
		SELECT task_title, task_description, ai_employee_name, ai_employee_role,
		       team_name, department_name, capabilities
		FROM execution_contexts
		WHERE execution_id = $1 AND workspace_id = $2`, executionID, workspaceID).
		Scan(&title, &desc, &m.AIEmployeeName, &m.AIEmployeeRole, &team, &dept, pq.Array(&caps))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, execution.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.TaskTitle, m.TaskDescription = title.String, desc.String
	m.TeamName, m.DepartmentName = team.String, dept.String
	m.Capabilities = caps
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &m, nil
}

func nullableBytes(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

const executionSelect = `
	SELECT e.id, e.workspace_id, e.ai_employee_id, e.task_id, e.status,
	       e.attempt, e.max_attempts, e.idempotency_key, e.request_hash,
	       e.lease_owner, e.lease_generation, e.lease_expires_at, e.next_retry_at,
	       e.last_error_code, e.retryable, e.error_detail, e.result_json,
	       e.completed_at, e.failed_at, e.cancelled_at, e.created_at, e.updated_at
	FROM ai_executions e`

func scanExecution(r rowScanner) (*execution.Execution, error) {
	var (
		e                            execution.Execution
		id, wsID, empID              uuid.UUID
		taskID                       uuid.NullUUID
		status                       string
		idemKey                      sql.NullString
		leaseOwner                   sql.NullString
		leaseExpires, nextRet        sql.NullTime
		lastErr                      sql.NullInt64
		retryable                    sql.NullBool
		errDetail                    sql.NullString
		result                       []byte
		completed, failed, cancelled sql.NullTime
	)
	if err := r.Scan(&id, &wsID, &empID, &taskID, &status, &e.Attempt, &e.MaxAttempts,
		&idemKey, &e.RequestHash, &leaseOwner, &e.LeaseGeneration, &leaseExpires, &nextRet,
		&lastErr, &retryable, &errDetail, &result, &completed, &failed, &cancelled,
		&e.CreatedAt, &e.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, execution.ErrNotFound
		}
		return nil, err
	}
	e.ID, e.WorkspaceID, e.AIEmployeeID = id, wsID, empID
	e.Status = execution.Status(status)
	if taskID.Valid {
		e.TaskID = &taskID.UUID
	}
	if idemKey.Valid {
		e.IdempotencyKey = &idemKey.String
	}
	if leaseOwner.Valid {
		e.LeaseOwner = &leaseOwner.String
	}
	if leaseExpires.Valid {
		e.LeaseExpiresAt = &leaseExpires.Time
	}
	if nextRet.Valid {
		e.NextRetryAt = &nextRet.Time
	}
	if lastErr.Valid {
		e.LastErrorCode = execution.ErrorCode(int(lastErr.Int64))
	}
	if retryable.Valid {
		e.Retryable = &retryable.Bool
	}
	if errDetail.Valid {
		e.ErrorDetail = &errDetail.String
	}
	e.ResultJSON = result
	if completed.Valid {
		e.CompletedAt = &completed.Time
	}
	if failed.Valid {
		e.FailedAt = &failed.Time
	}
	if cancelled.Valid {
		e.CancelledAt = &cancelled.Time
	}
	return &e, nil
}

func scanFenced(r rowScanner) (*execution.FencedExecution, error) {
	var f execution.FencedExecution
	var id, wsID, empID, retryID uuid.UUID
	var owner string
	if err := r.Scan(&id, &wsID, &empID, &owner, &f.LeaseGeneration, &f.LeaseExpiresAt,
		&f.Attempt, &f.MaxAttempts, &retryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, execution.ErrNotFound
		}
		return nil, err
	}
	f.ExecutionID, f.WorkspaceID, f.AIEmployeeID, f.LeaseOwner = id, wsID, empID, owner
	f.RetryID = retryID
	return &f, nil
}

func scanFencedWithClassification(r rowScanner) (*execution.FencedExecution, error) {
	var f execution.FencedExecution
	var id, wsID, empID, retryID uuid.UUID
	var owner string
	var lastErr sql.NullInt64
	var retryable sql.NullBool
	var nextRet sql.NullTime
	if err := r.Scan(&id, &wsID, &empID, &owner, &f.LeaseGeneration, &f.LeaseExpiresAt,
		&f.Attempt, &f.MaxAttempts, &retryID, &lastErr, &retryable, &nextRet); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, execution.ErrNotFound
		}
		return nil, err
	}
	f.ExecutionID, f.WorkspaceID, f.AIEmployeeID, f.LeaseOwner = id, wsID, empID, owner
	f.RetryID = retryID
	if lastErr.Valid {
		code := execution.ErrorCode(int(lastErr.Int64))
		f.LastErrorCode = &code
	}
	if retryable.Valid {
		v := retryable.Bool
		f.Retryable = &v
	}
	if nextRet.Valid {
		f.NextRetryAt = &nextRet.Time
	}
	return &f, nil
}
