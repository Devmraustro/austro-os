package task

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// This file is the bounded read side of the task domain.
//
// It exists separately from store.go because an unbounded listing and a bounded
// one are different contracts, and only one of them is safe to put behind an
// HTTP route. A task table grows for as long as a workspace exists, so a read
// that returns every row is a denial-of-service vector as much as a correctness
// problem: the whole tenant history has to be scanned, serialized and shipped.

// ErrInvalidCursor reports a pagination cursor this package did not issue.
var ErrInvalidCursor = errors.New("task: invalid cursor")

// Bounds on a task listing.
const (
	// DefaultPageSize applies when the caller asks for nothing.
	DefaultPageSize = 50
	// MaxPageSize is the ceiling a caller may request. It is clamped rather than
	// rejected, so the ceiling cannot be probed for a different error path.
	MaxPageSize = 200
)

// ListQuery describes one bounded task listing. The zero value means "the most
// recent page, unfiltered".
type ListQuery struct {
	// Status filters to one lifecycle stage. Nil means every stage.
	Status *Status
	// Limit is clamped to [1, MaxPageSize]; zero means DefaultPageSize.
	Limit int
	// Before is the keyset cursor. Only tasks sorting strictly after it in the
	// listing order are returned. The zero Cursor means "start at the newest".
	Before Cursor
}

// Normalize applies the bounds. Unlike the audit reader it never has to reject
// anything: every field here is either a validated enum or an integer that can
// be clamped, so there is no malformed-but-plausible input to refuse.
func (q *ListQuery) Normalize() {
	if q.Limit <= 0 {
		q.Limit = DefaultPageSize
	}
	if q.Limit > MaxPageSize {
		q.Limit = MaxPageSize
	}
}

// Cursor is a position in the listing order, which is (created_at DESC, id DESC).
//
// Keyset pagination is used rather than an offset because an offset is wrong the
// moment anything is inserted or deleted mid-walk: rows shift under the reader
// and a page is skipped or repeated. A keyset cursor names the last row the
// caller saw, so concurrent inserts cannot move it.
//
// created_at alone would not be a safe key -- two tasks created in the same
// microsecond would tie, and a tie in the sort makes the page boundary
// arbitrary. The id breaks the tie, which is why the ordering and the cursor
// both carry it.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
	// Set distinguishes "no cursor" from a cursor that happens to hold zero
	// values, so an empty query is never mistaken for a position.
	Set bool
}

// EncodeCursor renders the cursor a client should pass back to fetch the page
// after t. The format is deliberate and readable rather than opaque: it carries
// nothing the client did not already receive in the task itself.
func EncodeCursor(t *Task) string {
	if t == nil {
		return ""
	}
	return strconv.FormatInt(t.CreatedAt.UnixNano(), 10) + "." + t.ID.String()
}

// DecodeCursor parses a cursor this package issued. Anything else is an error
// rather than a silent reset to the first page, because a caller whose cursor
// was mangled should be told, not quietly handed the newest page again and left
// to believe it had paged forward.
func DecodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	ts, id, found := strings.Cut(s, ".")
	if !found {
		return Cursor{}, ErrInvalidCursor
	}
	nano, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	// Reject a cursor claiming a time the database could not have stored. The
	// column is a TIMESTAMP with microsecond precision, so anything outside the
	// range PostgreSQL accepts would fail at query time as a server error.
	if nano < -6795364578871345152 || nano > 2534023007999999999 {
		return Cursor{}, ErrInvalidCursor
	}
	idv, err := uuid.Parse(id)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{CreatedAt: time.Unix(0, nano).UTC(), ID: idv, Set: true}, nil
}

// Page is one bounded listing result.
type Page struct {
	Tasks []*Task
	// NextCursor is the cursor for the following page. It is empty when the page
	// came back short, which is how the caller knows it has reached the end.
	NextCursor string
	// Limit is the page size actually applied, which may be smaller than the one
	// requested.
	Limit int
}

// ValidateStatus reports whether s names a lifecycle stage, for callers that
// hold a raw string from a request and need to reject an unknown one before it
// reaches a query.
func ValidateStatus(s string) (Status, error) {
	st := Status(s)
	if !ValidStatus(st) {
		return "", fmt.Errorf("%w: %q is not a task status", ErrStatus, s)
	}
	return st, nil
}
