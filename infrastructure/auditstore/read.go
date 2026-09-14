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

// Reader serves bounded audit reads and implements the audit.Reader port, so
// the API layer depends on the domain interface rather than on this package.
//
// It is separate from Store because the two have different privileges and
// different failure modes: Store owns the chain head and appends to it, while
// Reader only ever issues SELECTs.
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

// compile-time proof that the concrete reader satisfies the domain port.
var _ audit.Reader = (*Reader)(nil)

// NewReader returns a reader over the given pool.
func NewReader(db *sql.DB) *Reader { return &Reader{db: db} }

const eventViewColumns = `
	SELECT seq, event_id, COALESCE(timestamp_canonical, ''), timestamp,
	       workspace_id, trace_id, span_id,
	       actor_type, actor_id, target_type, target_id,
	       event_type, outcome, constitutional_principle, genesis
	FROM audit_events`

// List returns one page in organization scope, newest first.
//
// Ordering is seq DESC and nothing else. seq is unique, so the order is total:
// two concurrent appends cannot produce a tie that reshuffles between pages,
// which is the failure that makes offset pagination lose or repeat rows.
func (r *Reader) List(ctx context.Context, q audit.Query) ([]audit.EventView, error) {
	if err := q.Normalize(); err != nil {
		return nil, err
	}
	query, args := r.buildQuery(q)
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
func (r *Reader) ListForWorkspace(ctx context.Context, workspaceID uuid.UUID, q audit.Query) ([]audit.EventView, error) {
	if err := q.Normalize(); err != nil {
		return nil, err
	}
	// The workspace is forced, not merely defaulted: an organization-scoped
	// filter arriving here must not widen a tenant-scoped read.
	q.Workspace = &workspaceID

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		"SELECT set_config('app.current_workspace', $1, true)", workspaceID.String()); err != nil {
		return nil, err
	}

	query, args := r.buildQuery(q)
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
// why it is founder-only.
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

// buildQuery assembles the statement. Every filter value is a bound parameter,
// so none of it can reach the SQL text; the only interpolation is the integer
// limit, which Normalize has already clamped.
func (r *Reader) buildQuery(q audit.Query) (string, []interface{}) {
	var (
		conds []string
		args  []interface{}
	)
	if q.Workspace != nil {
		args = append(args, *q.Workspace)
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
	query := eventViewColumns
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	return query + ` ORDER BY seq DESC LIMIT ` + itoa(q.Limit), args
}

func collectEventViews(rows *sql.Rows) ([]audit.EventView, error) {
	out := make([]audit.EventView, 0, audit.DefaultPageSize)
	for rows.Next() {
		var (
			v         audit.EventView
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
		// RFC3339Nano is spelled out rather than reusing this package's unexported
		// constant so the wire format stays identical to the hashed one.
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
