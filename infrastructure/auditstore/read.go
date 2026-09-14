package auditstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"austro-os/internal/audit"

	"github.com/google/uuid"
)

// EventView is the read model the audit API exposes.
//
// It is deliberately NOT audit.AuditEvent. That struct is bound into the hash
// chain and is what Verify replays, so adding a field to it for presentation
// would change what the chain means. This view is a projection chosen for
// display, and it omits four columns on purpose:
//
//   - hash_chain_value and digital_signature are internal cryptographic
//     material. Publishing them invites an attacker to study the construction,
//     and they tell an operator nothing a boolean "chain intact" does not.
//   - outcome_details and permissions_checked are free-form JSONB written by
//     whatever code recorded the event. Nothing constrains their contents, so
//     they are exactly where a caller's input, an error string or a credential
//     fragment would land. They stay out of every API response.
//
// Everything that remains is a bounded identifier, an enum-like string the
// writer controls, or a timestamp.
type EventView struct {
	// Seq is the chain position. It is unique and monotonic, which makes it
	// both a stable sort key and a safe pagination cursor.
	Seq int64 `json:"seq"`
	// EventID is the event's own identity, distinct from its chain position.
	EventID   uuid.UUID  `json:"event_id"`
	Timestamp time.Time  `json:"timestamp"`
	Workspace *uuid.UUID `json:"workspace_id,omitempty"`
	TraceID   *uuid.UUID `json:"trace_id,omitempty"`
	SpanID    *uuid.UUID `json:"span_id,omitempty"`
	ActorType string     `json:"actor_type"`
	ActorID   *uuid.UUID `json:"actor_id,omitempty"`
	// TargetType and TargetID identify what the event acted on.
	TargetType string     `json:"target_type"`
	TargetID   *uuid.UUID `json:"target_id,omitempty"`
	EventType  string     `json:"event_type"`
	Outcome    string     `json:"outcome"`
	// ConstitutionalPrinciple ties the event to the principle it enforces.
	ConstitutionalPrinciple string `json:"constitutional_principle"`
	// Genesis marks the chain root.
	Genesis bool `json:"genesis,omitempty"`
}

// Bounds on an audit read. The audit table is append-only and grows without
// limit, so an unbounded read is a denial-of-service vector as much as a
// correctness problem: every row has to be scanned, serialized and shipped.
const (
	// DefaultPageSize applies when the caller asks for nothing.
	DefaultPageSize = 50
	// MaxPageSize is the ceiling a caller can request. It is a hard clamp, not
	// a suggestion: a larger limit is reduced rather than rejected, so a client
	// cannot probe the ceiling for an error path.
	MaxPageSize = 200
	// maxFilterRunes bounds the free-text filters. They are exact matches
	// against indexed, enum-like columns, so anything longer is meaningless
	// input rather than a legitimate value.
	maxFilterRunes = 64
)

// Query describes one bounded audit read. Every field is optional; the zero
// value is "the most recent page, unfiltered".
type Query struct {
	// WorkspaceID restricts the read to one workspace. Callers that are not
	// organization-scoped must set it, and the handler derives it from the
	// verified claims rather than from the request.
	WorkspaceID *uuid.UUID
	// EventType, Outcome and ActorType are exact matches. Empty means unset.
	EventType string
	Outcome   string
	ActorType string
	// BeforeSeq is the pagination cursor: only rows with seq < BeforeSeq are
	// returned. Zero means "start at the newest event".
	BeforeSeq int64
	// Limit is clamped to [1, MaxPageSize]; zero means DefaultPageSize.
	Limit int
}

// Normalize applies the bounds and rejects filter values that cannot be
// legitimate. It returns an error rather than silently truncating a filter,
// because a silently shortened filter matches different rows than the caller
// asked for -- a wrong answer instead of a refused one.
func (q *Query) Normalize() error {
	if q.Limit <= 0 {
		q.Limit = DefaultPageSize
	}
	if q.Limit > MaxPageSize {
		q.Limit = MaxPageSize
	}
	for name, v := range map[string]string{
		"event_type": q.EventType,
		"outcome":    q.Outcome,
		"actor_type": q.ActorType,
	} {
		if len([]rune(v)) > maxFilterRunes {
			return fmt.Errorf("auditstore: %s filter exceeds %d characters", name, maxFilterRunes)
		}
	}
	return nil
}

// Reader serves bounded audit reads. It is separate from Store because the two
// have different privileges and different failure modes: Store owns the chain
// head and appends to it, while Reader only ever issues SELECTs.
//
// Two access shapes exist and they are not interchangeable:
//
//   - List runs against the administrative handle. That role is named by
//     audit_org_policy, which is what makes an organization-wide view possible
//     at all. It must therefore only ever be reached by an organization-scoped
//     caller; the handler enforces that with an explicit role check.
//   - ListForWorkspace runs against the unprivileged runtime handle inside a
//     transaction that binds app.current_workspace, so audit_workspace_policy
//     confines the read in the database rather than in Go. The explicit
//     workspace_id predicate is kept as defense-in-depth, matching every other
//     store in this repository.
type Reader struct {
	db *sql.DB
}

// NewReader returns a reader over the given pool.
func NewReader(db *sql.DB) *Reader { return &Reader{db: db} }

const eventViewColumns = `
	SELECT seq, event_id, COALESCE(timestamp_canonical, '') , timestamp,
	       workspace_id, trace_id, span_id,
	       actor_type, actor_id, target_type, target_id,
	       event_type, outcome, constitutional_principle, genesis
	FROM audit_events`

// List returns one page in organization scope, newest first.
//
// Ordering is seq DESC and nothing else. seq is unique, so the order is total:
// two concurrent appends cannot produce a tie that reshuffles between pages,
// which is the failure that makes offset pagination lose or repeat rows.
func (r *Reader) List(ctx context.Context, q Query) ([]EventView, error) {
	if err := q.Normalize(); err != nil {
		return nil, err
	}
	where, args := buildAuditWhere(q)
	query := eventViewColumns + where + ` ORDER BY seq DESC LIMIT ` + itoa(q.Limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEventViews(rows)
}

// ListForWorkspace returns one page confined to a single workspace. It binds
// the workspace on the connection first, so row-level security applies; see the
// Reader doc comment for why this must run on the runtime handle.
func (r *Reader) ListForWorkspace(ctx context.Context, workspaceID uuid.UUID, q Query) ([]EventView, error) {
	if err := q.Normalize(); err != nil {
		return nil, err
	}
	// The workspace is forced, not merely defaulted: an organization-scoped
	// filter arriving here must not widen a tenant-scoped read.
	q.WorkspaceID = &workspaceID

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('app.current_workspace', $1, true)", workspaceID.String()); err != nil {
		return nil, err
	}

	where, args := buildAuditWhere(q)
	query := eventViewColumns + where + ` ORDER BY seq DESC LIMIT ` + itoa(q.Limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views, err := collectEventViews(rows)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return views, nil
}

// VerifyChain re-reads the persisted chain and recomputes every link, reporting
// whether it is intact and how many events were checked.
//
// It is independent of the writer's in-memory head by construction: a Reader
// holds no head, so this cannot be satisfied by anything the appending code
// remembers. That is the property that lets it catch an edit made behind the
// application's back.
//
// It reads the whole table, which is why it is a separate route from List and
// why it is founder-only. A bounded proof of a hash chain is not a thing; either
// every link is recomputed or the answer means nothing.
func (r *Reader) VerifyChain(ctx context.Context) (bool, int, error) {
	rows, err := r.db.QueryContext(ctx, selectChainSQL+` ORDER BY seq ASC`)
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	var events []*audit.AuditEvent
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return false, 0, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return false, 0, err
	}
	return audit.VerifyHashChain(events), len(events), nil
}

// buildAuditWhere assembles the predicate. Every value is a bound parameter, so
// a filter value can never reach the SQL text; the only string interpolation in
// this file is the integer limit, which has already been clamped.
func buildAuditWhere(q Query) (string, []interface{}) {
	var (
		conds []string
		args  []interface{}
	)
	if q.WorkspaceID != nil {
		args = append(args, *q.WorkspaceID)
		conds = append(conds, fmt.Sprintf("workspace_id = $%d", len(args)))
	}
	if q.EventType != "" {
		args = append(args, q.EventType)
		conds = append(conds, fmt.Sprintf("event_type = $%d", len(args)))
	}
	if q.Outcome != "" {
		args = append(args, q.Outcome)
		conds = append(conds, fmt.Sprintf("outcome = $%d", len(args)))
	}
	if q.ActorType != "" {
		args = append(args, q.ActorType)
		conds = append(conds, fmt.Sprintf("actor_type = $%d", len(args)))
	}
	if q.BeforeSeq > 0 {
		args = append(args, q.BeforeSeq)
		conds = append(conds, fmt.Sprintf("seq < $%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func collectEventViews(rows *sql.Rows) ([]EventView, error) {
	out := make([]EventView, 0, DefaultPageSize)
	for rows.Next() {
		var (
			v         EventView
			canonical string
		)
		if err := rows.Scan(&v.Seq, &v.EventID, &canonical, &v.Timestamp,
			&v.Workspace, &v.TraceID, &v.SpanID,
			&v.ActorType, &v.ActorID, &v.TargetType, &v.TargetID,
			&v.EventType, &v.Outcome, &v.ConstitutionalPrinciple, &v.Genesis); err != nil {
			return nil, err
		}
		// timestamp_canonical holds the RFC3339Nano instant the hash binds, and
		// is the value an operator should see; the TIMESTAMP column is only the
		// storage representation. Prefer the canonical text when it parses.
		// RFC3339Nano is spelled out rather than reusing internal/audit's
		// unexported constant, matching how this package's own scanEvent does
		// it, so the wire format stays identical to the hashed one.
		if canonical != "" {
			if t, err := time.Parse(time.RFC3339Nano, canonical); err == nil {
				v.Timestamp = t
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// itoa renders an already-clamped, non-negative limit. It exists so the LIMIT
// interpolation cannot be mistaken for a place where caller input reaches SQL.
func itoa(n int) string {
	if n <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
