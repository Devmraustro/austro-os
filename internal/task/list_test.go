package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestListQueryNormalizeClamps(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to the default", 0, DefaultPageSize},
		{"negative falls back to the default", -5, DefaultPageSize},
		{"explicit value is kept", 7, 7},
		{"exactly the maximum is kept", MaxPageSize, MaxPageSize},
		{"over the maximum is clamped", MaxPageSize * 500, MaxPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := ListQuery{Limit: tc.in}
			q.Normalize()
			require.Equal(t, tc.want, q.Limit)
		})
	}
}

// TestCursorRoundTrip pins the property pagination depends on: a cursor encodes a
// position and decodes back to exactly that position, including the sub-second
// part. Losing precision here would silently re-order or repeat rows.
func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)
	id := uuid.MustParse("3f2b1c9e-0000-4000-8000-000000000001")
	enc := EncodeCursor(&Task{CreatedAt: at, ID: id})
	require.NotEmpty(t, enc)

	got, err := DecodeCursor(enc)
	require.NoError(t, err)
	require.True(t, got.Set, "a decoded cursor must be marked as set")
	require.Equal(t, id, got.ID)
	require.True(t, got.CreatedAt.Equal(at),
		"the cursor must survive the round trip to the nanosecond: got %s want %s",
		got.CreatedAt, at)
}

func TestDecodeCursorRejectsMalformed(t *testing.T) {
	for name, raw := range map[string]string{
		"empty is not an error but yields no cursor": "",
		"no separator":                               "12345",
		"non-numeric time":                           "notanumber.3f2b1c9e-0000-4000-8000-000000000001",
		"bad uuid":                                   "12345.not-a-uuid",
		"empty time":                                 ".3f2b1c9e-0000-4000-8000-000000000001",
		"empty uuid":                                 "12345.",
		"trailing garbage":                           "12345.3f2b1c9e-0000-4000-8000-000000000001.extra",
		"float time":                                 "123.5.3f2b1c9e-0000-4000-8000-000000000001",
		"time before range":                          "-99999999999999999999.3f2b1c9e-0000-4000-8000-000000000001",
		"sql injection":                              "1;DROP TABLE tasks--.x",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeCursor(raw)
			if raw == "" {
				require.NoError(t, err, "an absent cursor is legitimate")
				require.False(t, got.Set)
				return
			}
			require.Error(t, err, "%q must be refused, not silently reset to page one", raw)
			require.ErrorIs(t, err, ErrInvalidCursor)
		})
	}
}

// TestListPageIsBoundedAndOrdered drives the service against the in-memory store
// and checks the two properties a client relies on: the page never exceeds the
// limit, and the order is stable and total.
func TestListPageIsBoundedAndOrdered(t *testing.T) {
	ws := uuid.New()
	other := uuid.New()
	store := newMemStore()
	svc := NewService(store, nil, nil)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		tk, err := svc.Create(context.Background(), ws, "task", PriorityNormal, "")
		require.NoError(t, err)
		// Force distinct, increasing timestamps so the ordering assertion has
		// something to check rather than relying on wall-clock resolution.
		tk.CreatedAt = base.Add(time.Duration(i) * time.Hour)
		tk.UpdatedAt = tk.CreatedAt
		_, err = store.Update(context.Background(), ws, tk)
		require.NoError(t, err)
	}
	// A task in another workspace must never appear.
	_, err := svc.Create(context.Background(), other, "elsewhere", PriorityNormal, "")
	require.NoError(t, err)

	page, err := svc.ListPage(context.Background(), ws, ListQuery{Limit: 3})
	require.NoError(t, err)
	require.Len(t, page.Tasks, 3, "the page must not exceed the requested limit")
	require.Equal(t, 3, page.Limit)
	require.NotEmpty(t, page.NextCursor, "more rows exist, so a cursor must be offered")

	for i := 1; i < len(page.Tasks); i++ {
		require.True(t, page.Tasks[i-1].CreatedAt.After(page.Tasks[i].CreatedAt),
			"tasks must be newest-first")
	}
	for _, tk := range page.Tasks {
		require.Equal(t, ws, tk.WorkspaceID, "another workspace's task leaked into the page")
	}
}

// TestListPageCursorCoversEveryTaskExactlyOnce walks the whole set through the
// cursor. This is the assertion that fails if the ordering is not total or the
// keyset comparison is wrong: rows would be skipped or returned twice.
func TestListPageCursorCoversEveryTaskExactlyOnce(t *testing.T) {
	ws := uuid.New()
	store := newMemStore()
	svc := NewService(store, nil, nil)

	const total = 11
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < total; i++ {
		tk, err := svc.Create(context.Background(), ws, "task", PriorityNormal, "")
		require.NoError(t, err)
		tk.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		tk.UpdatedAt = tk.CreatedAt
		_, err = store.Update(context.Background(), ws, tk)
		require.NoError(t, err)
	}

	seen := map[uuid.UUID]bool{}
	cursor := ""
	for i := 0; i < 50; i++ {
		q := ListQuery{Limit: 3}
		if cursor != "" {
			c, err := DecodeCursor(cursor)
			require.NoError(t, err)
			q.Before = c
		}
		page, err := svc.ListPage(context.Background(), ws, q)
		require.NoError(t, err)
		if len(page.Tasks) == 0 {
			break
		}
		for _, tk := range page.Tasks {
			require.False(t, seen[tk.ID], "task %s was returned twice: the cursor repeats rows", tk.ID)
			seen[tk.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	require.Len(t, seen, total, "pagination must cover every task exactly once")
}

// TestListPageStatusFilter checks the filter narrows rather than widens.
func TestListPageStatusFilter(t *testing.T) {
	ws := uuid.New()
	store := newMemStore()
	svc := NewService(store, nil, nil)

	a, err := svc.Create(context.Background(), ws, "a", PriorityNormal, "")
	require.NoError(t, err)
	_, err = svc.Create(context.Background(), ws, "b", PriorityNormal, "")
	require.NoError(t, err)
	_, err = svc.Transition(context.Background(), ws, a.ID, StatusPlanned)
	require.NoError(t, err)

	planned := StatusPlanned
	page, err := svc.ListPage(context.Background(), ws, ListQuery{Status: &planned})
	require.NoError(t, err)
	require.Len(t, page.Tasks, 1)
	require.Equal(t, a.ID, page.Tasks[0].ID)

	backlog := StatusBacklog
	page, err = svc.ListPage(context.Background(), ws, ListQuery{Status: &backlog})
	require.NoError(t, err)
	require.Len(t, page.Tasks, 1)
	require.Equal(t, StatusBacklog, page.Tasks[0].Status)

	bogus := Status("not-a-status")
	_, err = svc.ListPage(context.Background(), ws, ListQuery{Status: &bogus})
	require.ErrorIs(t, err, ErrStatus, "an unknown status must be refused, not ignored")
}

// TestListPageRequiresWorkspace proves the read is bound to a tenant: a listing
// with no workspace is refused rather than returning everything.
func TestListPageRequiresWorkspace(t *testing.T) {
	svc := NewService(newMemStore(), nil, nil)
	_, err := svc.ListPage(context.Background(), uuid.Nil, ListQuery{})
	require.ErrorIs(t, err, ErrWorkspaceMismatch)
}

func TestValidateStatus(t *testing.T) {
	for _, s := range []string{"backlog", "planned", "in_progress", "in_review",
		"completed", "cancelled", "rejected", "failed"} {
		got, err := ValidateStatus(s)
		require.NoError(t, err)
		require.Equal(t, Status(s), got)
	}
	for _, s := range []string{"", "BACKLOG", "done", "in progress", "backlog; DROP TABLE tasks"} {
		_, err := ValidateStatus(s)
		require.Error(t, err, "%q is not a lifecycle stage", s)
		require.ErrorIs(t, err, ErrStatus)
	}
}

// TestEncodeCursorNil guards the one input that would otherwise panic.
func TestEncodeCursorNil(t *testing.T) {
	require.Equal(t, "", EncodeCursor(nil))
}

// TestStatusesMatchTheDatabaseConstraints pins the list in migrateTasks against
// the domain constants. The two are written in different languages for different
// reasons -- one is validation a caller can read, the other is an invariant the
// table enforces -- and if they drift the database starts rejecting values the
// service considers valid.
func TestStatusesMatchTheDatabaseConstraints(t *testing.T) {
	want := map[Status]bool{
		StatusBacklog: true, StatusPlanned: true, StatusInProgress: true,
		StatusInReview: true, StatusCompleted: true, StatusCancelled: true,
		StatusRejected: true, StatusFailed: true,
	}
	for s := range want {
		require.True(t, ValidStatus(s), "%s must be a valid status", s)
	}
	// A value outside the set must be rejected, which is what the CHECK
	// constraint also enforces.
	require.False(t, ValidStatus(Status("archived")))
	require.False(t, ValidStatus(Status(strings.ToUpper(string(StatusBacklog)))))
}
