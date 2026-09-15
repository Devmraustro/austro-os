package knowledge

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// This file is the bounded read side of the knowledge domain.
//
// It exists separately from store.go because an unbounded listing and a bounded
// one are different contracts, and only one of them is safe to put behind an
// HTTP route. The previous List returned every document in the workspace with no
// LIMIT and no ORDER BY, which is an unbounded response and a non-deterministic
// one at the same time: a knowledge base grows for as long as a workspace
// exists, so the whole of it would have to be scanned, serialized and shipped,
// and two identical requests could return it in different orders.

// ErrInvalidCursor reports a pagination cursor this package did not issue.
var ErrInvalidCursor = errors.New("knowledge: invalid cursor")

// Bounds on a knowledge listing.
const (
	// DefaultPageSize applies when the caller asks for nothing.
	DefaultPageSize = 50
	// MaxPageSize is the ceiling a caller may request. It is clamped rather than
	// rejected, so the ceiling cannot be probed for a different error path.
	MaxPageSize = 200
	// MaxSearchResults is the ceiling on a similarity search. Search is a
	// separate bound from listing because it is ranked, not paged: the caller
	// wants the k nearest documents, not a walk through all of them.
	MaxSearchResults = 50
)

// ListQuery describes one bounded knowledge listing. The zero value means "the
// most recent page, every kind".
type ListQuery struct {
	// Kind filters to one document kind. Nil means every kind.
	Kind *Kind
	// Limit is clamped to [1, MaxPageSize]; zero means DefaultPageSize.
	Limit int
	// Before is the keyset cursor. Only documents sorting strictly after it in
	// the listing order are returned. The zero Cursor means "start at the newest".
	Before Cursor
}

// Normalize applies the bounds. Nothing here has to be rejected: the kind is a
// validated enum and the limit can always be clamped.
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
// caller saw, so a concurrent insert cannot move it.
//
// created_at alone would not be a safe key -- two documents created in the same
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
// after d. It carries nothing the client did not already receive in the document
// itself, so it is readable rather than opaque.
func EncodeCursor(d *Document) string {
	if d == nil {
		return ""
	}
	return strconv.FormatInt(d.CreatedAt.UnixNano(), 10) + "." + d.ID.String()
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
	// column is a TIMESTAMP with microsecond precision, so a value outside the
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
	Documents []*Document
	// NextCursor is the cursor for the following page. It is empty when the page
	// came back short, which is how the caller knows it has reached the end.
	NextCursor string
	// Limit is the page size actually applied, which may be smaller than the one
	// requested.
	Limit int
}

// ValidateKind reports whether s names a recognized document kind, for callers
// holding a raw string from a request that must be rejected before it reaches a
// query. It exists because ValidKind takes a Kind, and converting first would
// let an arbitrary string through as a typed value.
func ValidateKind(s string) (Kind, error) {
	k := Kind(s)
	if !ValidKind(k) {
		return "", fmt.Errorf("%w: %q is not a knowledge kind", ErrInvalidKind, s)
	}
	return k, nil
}

// NormalizeSearchLimit clamps a similarity-search limit into [1, MaxSearchResults].
// The service already fell back to 10 for an out-of-range value, but silently
// substituting a default hides a caller's mistake, so the boundary reports it
// instead and this only handles the clamp.
func NormalizeSearchLimit(limit int) int {
	if limit <= 0 {
		return 10
	}
	if limit > MaxSearchResults {
		return MaxSearchResults
	}
	return limit
}
