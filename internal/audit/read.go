package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// This file is the read side of the audit domain. It exists as a port so that
// the API layer can serve audit history without importing infrastructure: the
// dependency rule in this repository is that internal/ never reaches down into
// infrastructure/, and a handler that took a concrete Postgres reader would
// break it. infrastructure/auditstore implements Reader.

// EventView is the read model exposed to callers.
//
// It is deliberately NOT AuditEvent. That struct is bound into the hash chain
// and is what Verify replays, so adding a field to it for presentation would
// change what the chain means. This view is a projection chosen for display, and
// it omits four stored columns on purpose:
//
//   - hash_chain_value and digital_signature are internal cryptographic
//     material. Publishing them invites study of the construction, and they tell
//     an operator nothing a boolean "chain intact" does not.
//   - outcome_details and permissions_checked are free-form JSONB written by
//     whatever code recorded the event. Nothing constrains their contents, so
//     they are exactly where a caller's input, an error string or a credential
//     fragment would land. They stay out of every response.
//
// What remains is a bounded identifier, an enum-like string the writer controls,
// or a timestamp.
type EventView struct {
	// Seq is the chain position: unique and monotonic, which makes it both a
	// stable sort key and a safe pagination cursor.
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

// Bounds on an audit read. The table is append-only and grows without limit, so
// an unbounded read is a denial-of-service vector as much as a correctness
// problem: every row has to be scanned, serialized and shipped.
const (
	// DefaultPageSize applies when the caller asks for nothing.
	DefaultPageSize = 50
	// MaxPageSize is the ceiling a caller can request. It is a hard clamp rather
	// than a rejection, so a client cannot probe the ceiling for an error path.
	MaxPageSize = 200
	// maxFilterRunes bounds the free-text filters. They are exact matches
	// against indexed, enum-like columns, so anything longer is meaningless
	// input rather than a legitimate value.
	maxFilterRunes = 64
)

// Query describes one bounded audit read. Every field is optional; the zero
// value means "the most recent page, unfiltered".
type Query struct {
	// Workspace restricts the read to one workspace. Implementations serving a
	// tenant-scoped caller must force this from verified credentials rather than
	// honouring anything the client supplied.
	Workspace *uuid.UUID
	// EventType, Outcome and ActorType are exact matches. Empty means unset.
	EventType string
	Outcome   string
	ActorType string
	// BeforeSeq is the pagination cursor: only rows with a lower seq are
	// returned. Zero means "start at the newest event".
	BeforeSeq int64
	// Limit is clamped to [1, MaxPageSize]; zero means DefaultPageSize.
	Limit int
}

// Normalize applies the bounds and rejects filter values that cannot be
// legitimate. It errors rather than silently truncating, because a shortened
// filter matches different rows than the caller asked for -- a wrong answer
// instead of a refused one.
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
			return fmt.Errorf("audit: %s filter exceeds %d characters", name, maxFilterRunes)
		}
	}
	return nil
}

// Reader is the audit read port. Two access shapes exist and they are not
// interchangeable:
//
//   - List serves an organization-wide view. Whatever implements it must be
//     running as a principal the database allows to see every workspace, so the
//     caller must already have been authorized at organization scope.
//   - ListForWorkspace serves one tenant and must confine the read at the
//     storage layer, not by filtering afterwards.
//
// Implementations return newest-first pages ordered by a unique monotonic key,
// so a cursor can neither skip nor repeat a row.
type Reader interface {
	List(ctx context.Context, q Query) ([]EventView, error)
	ListForWorkspace(ctx context.Context, workspaceID uuid.UUID, q Query) ([]EventView, error)
	// VerifyChain recomputes every link and reports whether the chain is intact
	// and how many events were covered. It necessarily reads the whole history:
	// a bounded proof of a hash chain is not a thing.
	VerifyChain(ctx context.Context) (bool, int, error)
}
