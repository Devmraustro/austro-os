package knowledge

import (
	"context"
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
		{"zero becomes the default", 0, DefaultPageSize},
		{"negative becomes the default", -5, DefaultPageSize},
		{"small is untouched", 10, 10},
		{"exactly the ceiling is untouched", MaxPageSize, MaxPageSize},
		{"above the ceiling is clamped", MaxPageSize + 1, MaxPageSize},
		{"absurd is clamped, not rejected", 1000000, MaxPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := ListQuery{Limit: tc.in}
			q.Normalize()
			require.Equal(t, tc.want, q.Limit)
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	d := &Document{
		ID:        uuid.New(),
		CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 890000000, time.UTC),
	}
	encoded := EncodeCursor(d)
	require.NotEmpty(t, encoded)

	got, err := DecodeCursor(encoded)
	require.NoError(t, err)
	require.True(t, got.Set, "a decoded cursor must be marked set")
	require.Equal(t, d.ID, got.ID)
	require.True(t, got.CreatedAt.Equal(d.CreatedAt),
		"the timestamp must survive the round trip exactly: %v vs %v", got.CreatedAt, d.CreatedAt)
}

func TestEncodeCursorNil(t *testing.T) {
	require.Empty(t, EncodeCursor(nil), "no document means no cursor")
}

func TestDecodeCursorEmptyMeansStart(t *testing.T) {
	got, err := DecodeCursor("")
	require.NoError(t, err, "an empty cursor is the first page, not an error")
	require.False(t, got.Set, "and it must not be marked set")
}

func TestDecodeCursorRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"not-a-cursor",
		"123",
		".",
		"abc.def",
		"0.not-a-uuid",
		"0.00000000-0000-0000-0000-00000000000z",
		"99999999999999999999999." + uuid.NewString(), // time the column cannot hold
		"-99999999999999999999999." + uuid.NewString(),
		"1;DROP TABLE knowledge_documents--." + uuid.NewString(),
		" " + "." + uuid.NewString(),
	} {
		t.Run(in, func(t *testing.T) {
			_, err := DecodeCursor(in)
			require.ErrorIs(t, err, ErrInvalidCursor,
				"a cursor this package did not issue must be rejected, not reset to page one")
		})
	}
}

// TestValidateKind pins both directions: the three recognized kinds are
// accepted, and anything else is refused. A raw string converted straight to a
// Kind would let an arbitrary value through as though it were typed.
func TestValidateKind(t *testing.T) {
	for _, k := range []string{"document", "campaign_rule", "style_guide"} {
		t.Run("accepts "+k, func(t *testing.T) {
			got, err := ValidateKind(k)
			require.NoError(t, err)
			require.Equal(t, Kind(k), got)
		})
	}
	for _, k := range []string{"", "Document", "DOCUMENT", "note", " document", "campaign-rule"} {
		t.Run("rejects "+k, func(t *testing.T) {
			_, err := ValidateKind(k)
			require.ErrorIs(t, err, ErrInvalidKind)
		})
	}
}

func TestNormalizeSearchLimit(t *testing.T) {
	require.Equal(t, 10, NormalizeSearchLimit(0), "an unset limit falls back to 10")
	require.Equal(t, 10, NormalizeSearchLimit(-3))
	require.Equal(t, 5, NormalizeSearchLimit(5))
	require.Equal(t, MaxSearchResults, NormalizeSearchLimit(MaxSearchResults))
	require.Equal(t, MaxSearchResults, NormalizeSearchLimit(MaxSearchResults+500))
}

// TestMaxContentLengthMatchesTheSchema keeps the domain cap and the value the
// OpenAPI document advertises from drifting apart. The two are written in
// different places for good reason -- one is enforced in Go, the other described
// to clients -- so the link between them has to be asserted somewhere.
func TestMaxContentLengthMatchesTheSchema(t *testing.T) {
	require.Equal(t, 64*1024, MaxContentLength)
	require.Equal(t, MaxContentLength, maxContentLength,
		"the exported and internal spellings must be the same value")
}

// docAt builds a document with a fixed creation time so ordering assertions do
// not depend on the clock.
func docAt(ws uuid.UUID, at time.Time) *Document {
	return &Document{
		ID:          uuid.New(),
		WorkspaceID: ws,
		Kind:        KindDocument,
		Title:       "t",
		Content:     "c",
		CreatedAt:   at,
		UpdatedAt:   at,
	}
}

func TestListPageIsBoundedAndOrdered(t *testing.T) {
	ws := uuid.New()
	store := newFakeStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		_, err := store.Upsert(context.TODO(), docAt(ws, base.Add(time.Duration(i)*time.Minute)))
		require.NoError(t, err)
	}

	page, err := store.ListPage(context.TODO(), ws, ListQuery{Limit: 3})
	require.NoError(t, err)
	require.Len(t, page.Documents, 3, "a page must never exceed the requested limit")
	require.Equal(t, 3, page.Limit)
	require.NotEmpty(t, page.NextCursor, "more rows exist, so a cursor must be offered")

	for i := 1; i < len(page.Documents); i++ {
		prev, cur := page.Documents[i-1], page.Documents[i]
		require.True(t, prev.CreatedAt.After(cur.CreatedAt),
			"documents must be newest first")
	}
}

// TestListPageCursorCoversEveryDocumentExactlyOnce is the property keyset
// pagination exists to provide. An off-by-one in the boundary comparison shows
// up here as a repeated or a skipped row, and nowhere else.
func TestListPageCursorCoversEveryDocumentExactlyOnce(t *testing.T) {
	ws := uuid.New()
	store := newFakeStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const total = 11
	// Two documents share a timestamp on purpose: the id tie-break is what makes
	// the page boundary exact when created_at ties, and that is only exercised if
	// a tie actually lands on a boundary.
	for i := 0; i < total; i++ {
		at := base.Add(time.Duration(i/2) * time.Minute)
		_, err := store.Upsert(context.TODO(), docAt(ws, at))
		require.NoError(t, err)
	}

	seen := map[uuid.UUID]int{}
	cursor := ""
	for i := 0; i < 100; i++ {
		q := ListQuery{Limit: 2}
		if cursor != "" {
			c, err := DecodeCursor(cursor)
			require.NoError(t, err)
			q.Before = c
		}
		page, err := store.ListPage(context.TODO(), ws, q)
		require.NoError(t, err)
		if len(page.Documents) == 0 {
			break
		}
		for _, d := range page.Documents {
			seen[d.ID]++
			require.Equal(t, ws, d.WorkspaceID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	require.Len(t, seen, total, "every document must be returned")
	for id, n := range seen {
		require.Equal(t, 1, n, "document %s was returned %d times: the cursor repeats rows", id, n)
	}
}

func TestListPageKindFilter(t *testing.T) {
	ws := uuid.New()
	store := newFakeStore()
	base := time.Now().UTC()

	mk := func(kind Kind, at time.Time) {
		d := docAt(ws, at)
		d.Kind = kind
		_, err := store.Upsert(context.TODO(), d)
		require.NoError(t, err)
	}
	mk(KindDocument, base)
	mk(KindDocument, base.Add(time.Minute))
	mk(KindStyleGuide, base.Add(2*time.Minute))
	mk(KindCampaignRule, base.Add(3*time.Minute))

	kind := KindStyleGuide
	page, err := store.ListPage(context.TODO(), ws, ListQuery{Kind: &kind, Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Documents, 1)
	require.Equal(t, KindStyleGuide, page.Documents[0].Kind)

	page, err = store.ListPage(context.TODO(), ws, ListQuery{Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Documents, 4, "an unfiltered listing returns every kind")
}

func TestListPageRequiresWorkspace(t *testing.T) {
	store := newFakeStore()
	_, err := store.Upsert(context.TODO(), docAt(uuid.New(), time.Now().UTC()))
	require.NoError(t, err)

	// A different workspace sees nothing at all.
	page, err := store.ListPage(context.TODO(), uuid.New(), ListQuery{Limit: 50})
	require.NoError(t, err)
	require.Empty(t, page.Documents)
	require.Empty(t, page.NextCursor)
}

func TestServiceListPageRejectsNoWorkspace(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, &fakeEmbedder{}, nil, nil, 0)
	_, err := svc.ListPage(context.TODO(), uuid.Nil, ListQuery{})
	require.ErrorIs(t, err, ErrWorkspaceMismatch,
		"a listing with no workspace must be refused rather than run unscoped")
}

func TestServiceListPageNormalizesBeforeTheStore(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, &fakeEmbedder{}, nil, nil, 0)
	page, err := svc.ListPage(context.TODO(), uuid.New(), ListQuery{Limit: 100000})
	require.NoError(t, err)
	require.Equal(t, MaxPageSize, page.Limit,
		"the ceiling must be applied even when the store would have accepted more")
}
