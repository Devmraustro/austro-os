// Package auditstore is the persistent audit writer: the component that turns
// internal/audit's chain primitives into durable, verifiable records in
// PostgreSQL.
//
// It exists because the chain primitives alone proved nothing at runtime. The
// hashing and HMAC verification were correct and completely unwired, so
// audit_events was created on every boot and never written to: the only record
// of a security-relevant operation was a structured log line, which is neither
// append-only nor tamper-evident and does not survive a log rotation.
package auditstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"austro-os/internal/audit"
	logger "austro-os/internal/log"

	"github.com/google/uuid"
)

// advisoryLockKey namespaces the chain-appending lock. Appends must be serial:
// each event's hash is computed over its predecessor, so two concurrent writers
// that both read the same head would produce two events claiming the same
// parent and break the chain. The lock is transaction-scoped, so it is released
// whether the append commits or rolls back and cannot be leaked by a crash.
const advisoryLockKey = "austro.audit.chain"

// Store appends audit events to PostgreSQL and keeps the in-memory chain head
// that the next append links to.
type Store struct {
	db *sql.DB

	mu   sync.Mutex
	head *audit.AuditEvent
}

// compile-time proof that Store satisfies the audit contract main.go wires.
var _ audit.Sink = (*Store)(nil)

// New opens a Store on db and establishes the chain head.
//
// db must be a connection with organization-scoped read access to audit_events
// (the administrative role). The runtime role is deliberately append-only on
// that table, so it cannot read the chain it writes and therefore cannot be
// used to reconstruct or replay it.
//
// Establishing the head here is what makes the chain survive a restart: the
// writer resumes from the last persisted event rather than starting a second
// chain, and a database holding an existing chain is never re-genesis'd.
func New(ctx context.Context, db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	head, err := s.loadHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit chain head: %w", err)
	}
	if head == nil {
		// An empty table means a fresh deployment. The genesis root is
		// persisted like any other event, so a later restart finds it instead
		// of creating a second root and forking the chain.
		genesis := audit.GenesisEvent("Security by Design")
		if err := s.insert(ctx, genesis, nil); err != nil {
			return nil, fmt.Errorf("audit genesis: %w", err)
		}
		head = genesis
	}
	s.head = head
	return s, nil
}

// Head returns the current chain tip. It is a copy, so a caller cannot mutate
// the writer's state.
func (s *Store) Head() *audit.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.head == nil {
		return nil
	}
	cp := *s.head
	return &cp
}

// Append persists one audit record as the next link in the chain and returns
// the stored event.
//
// A failure is returned to the caller and nothing is written. Callers wiring
// this into a security-sensitive operation treat that as a failure of the
// operation itself: an action that cannot leave a durable audit record must not
// report success. The event is never half-persisted, because the row and the
// chain advance happen in one transaction.
func (s *Store) Append(ctx context.Context, rec audit.Record) (*audit.AuditEvent, error) {
	if rec.EventType == "" || rec.Outcome == "" || rec.Principle == "" {
		return nil, fmt.Errorf("audit record requires event type, outcome and principle")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Serialise against every other appender, in this process and any other,
	// then re-read the head under the lock. Re-reading matters for correctness
	// across replicas: the in-memory head is a cache, and trusting it would
	// fork the chain whenever a second process appended first.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("audit chain lock: %w", err)
	}
	head, err := loadHeadTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if head == nil {
		return nil, fmt.Errorf("audit chain has no genesis event")
	}
	s.head = head

	ev := audit.AppendEvent(
		head,
		rec.EventType,
		rec.ActorType,
		rec.ActorID,
		rec.TargetType,
		rec.TargetID,
		rec.Outcome,
		rec.Principle,
	)
	if err := insertTx(ctx, tx, ev, &rec); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("audit commit: %w", err)
	}
	s.head = ev

	logger.NewEntry("audit-event-persisted").
		With("event_id", ev.EventID.String()).
		With("event_type", ev.EventType).
		With("outcome", ev.Outcome).
		With("principle", ev.ConstitutionalPrinciple).
		With("trace_id", nullUUIDString(ev.TraceID)).
		Log()

	return ev, nil
}

// Chain reads the full persisted chain in order. It requires the
// organization-scoped read the administrative role provides; a workspace-scoped
// reader sees only its own rows and cannot verify a chain.
func (s *Store) Chain(ctx context.Context) ([]*audit.AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, selectChainSQL+` ORDER BY seq ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*audit.AuditEvent
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Verify re-reads the persisted chain from the database and reports whether it
// is intact and authentic. It is deliberately independent of the in-memory
// head: this is the check that would catch an edit made behind the
// application's back, so it must not trust anything the writer remembers.
func (s *Store) Verify(ctx context.Context) (bool, error) {
	events, err := s.Chain(ctx)
	if err != nil {
		return false, err
	}
	return audit.VerifyHashChain(events), nil
}

const selectChainSQL = `
	SELECT id, event_id, timestamp_canonical, trace_id, span_id,
	       actor_type, actor_id, target_type, target_id,
	       event_type, outcome, constitutional_principle,
	       digital_signature, hash_chain_parent, hash_chain_value, genesis
	FROM audit_events`

// loadHead returns the most recently appended event, or nil when the table is
// empty.
func (s *Store) loadHead(ctx context.Context) (*audit.AuditEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, advisoryLockKey); err != nil {
		return nil, err
	}
	head, err := loadHeadTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	return head, tx.Commit()
}

func loadHeadTx(ctx context.Context, tx *sql.Tx) (*audit.AuditEvent, error) {
	row := tx.QueryRowContext(ctx, selectChainSQL+` ORDER BY seq DESC LIMIT 1`)
	ev, err := scanRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ev, err
}

func (s *Store) insert(ctx context.Context, ev *audit.AuditEvent, rec *audit.Record) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, advisoryLockKey); err != nil {
		return err
	}
	if err := insertTx(ctx, tx, ev, rec); err != nil {
		return err
	}
	return tx.Commit()
}

const insertSQL = `
	INSERT INTO audit_events (
		id, event_id, workspace_id, timestamp, timestamp_canonical,
		trace_id, span_id, actor_type, actor_id, target_type, target_id,
		event_type, outcome, outcome_details, constitutional_principle,
		digital_signature, hash_chain_parent, hash_chain_value, genesis
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`

func insertTx(ctx context.Context, tx *sql.Tx, ev *audit.AuditEvent, rec *audit.Record) error {
	var details []byte
	if rec != nil {
		if redacted := audit.RedactDetails(rec.Details); len(redacted) > 0 {
			b, err := json.Marshal(redacted)
			if err != nil {
				return fmt.Errorf("audit details: %w", err)
			}
			details = b
		}
	}
	sig, err := json.Marshal(ev.DigitalSignature)
	if err != nil {
		return fmt.Errorf("audit signature: %w", err)
	}
	_, err = tx.ExecContext(ctx, insertSQL,
		ev.ID, ev.EventID,
		workspaceArg(rec),
		ev.Timestamp.UTC().Truncate(time.Microsecond),
		ev.Timestamp.UTC().Format(time.RFC3339Nano),
		nullUUID(ev.TraceID), nullUUID(ev.SpanID),
		ev.ActorType, nullUUID(ev.ActorID),
		ev.TargetType, nullUUID(ev.TargetID),
		ev.EventType, ev.Outcome, details, ev.ConstitutionalPrinciple,
		sig, nullUUID(ev.HashParent), ev.HashValue, ev.Genesis,
	)
	if err != nil {
		return fmt.Errorf("audit insert: %w", err)
	}
	return nil
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(rows *sql.Rows) (*audit.AuditEvent, error) { return scanRow(rows) }

func scanRow(s scanner) (*audit.AuditEvent, error) {
	var (
		ev        audit.AuditEvent
		canonical sql.NullString
		traceID   sql.Null[uuid.UUID]
		spanID    sql.Null[uuid.UUID]
		actorID   sql.Null[uuid.UUID]
		targetID  sql.Null[uuid.UUID]
		parent    sql.Null[uuid.UUID]
		sigJSON   []byte
	)
	if err := s.Scan(
		&ev.ID, &ev.EventID, &canonical, &traceID, &spanID,
		&ev.ActorType, &actorID, &ev.TargetType, &targetID,
		&ev.EventType, &ev.Outcome, &ev.ConstitutionalPrinciple,
		&sigJSON, &parent, &ev.HashValue, &ev.Genesis,
	); err != nil {
		return nil, err
	}

	// The canonical text is the authority for the timestamp, because the chain
	// hash binds nanosecond precision that a PostgreSQL timestamp column cannot
	// store. A row without it predates persistent audit and cannot be verified.
	if !canonical.Valid || canonical.String == "" {
		return nil, fmt.Errorf("audit event %s has no canonical timestamp", ev.EventID)
	}
	ts, err := time.Parse(time.RFC3339Nano, canonical.String)
	if err != nil {
		return nil, fmt.Errorf("audit event %s canonical timestamp: %w", ev.EventID, err)
	}
	ev.Timestamp = ts.UTC()
	ev.HashGeneratedAt = ev.Timestamp
	ev.SignatureGeneratedAt = ev.Timestamp

	if traceID.Valid {
		ev.TraceID = traceID.V
	}
	if spanID.Valid {
		ev.SpanID = spanID.V
	}
	if actorID.Valid {
		ev.ActorID = actorID.V
	}
	if targetID.Valid {
		ev.TargetID = targetID.V
	}
	if parent.Valid {
		ev.HashParent = parent.V
	}
	if len(sigJSON) > 0 {
		if err := json.Unmarshal(sigJSON, &ev.DigitalSignature); err != nil {
			return nil, fmt.Errorf("audit event %s signature: %w", ev.EventID, err)
		}
	}
	return &ev, nil
}

func workspaceArg(rec *audit.Record) any {
	if rec == nil || rec.WorkspaceID == nil {
		return nil
	}
	return *rec.WorkspaceID
}

func nullUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

func nullUUIDString(u uuid.UUID) string {
	if u == uuid.Nil {
		return ""
	}
	return u.String()
}
