package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeEmbedder struct {
	vec   []float32
	err   error
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, _ uuid.UUID, _ string, dim int) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.vec) == 0 {
		return make([]float32, dim), nil
	}
	return f.vec, nil
}

type fakeStore struct {
	stored []*Document
	byID   map[uuid.UUID]*Document
	err    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: map[uuid.UUID]*Document{}}
}

func (f *fakeStore) Upsert(_ context.Context, d *Document) (*Document, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.byID[d.ID] = d
	f.stored = append(f.stored, d)
	return d, nil
}

func (f *fakeStore) Get(_ context.Context, _, id uuid.UUID) (*Document, error) {
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

func (f *fakeStore) List(_ context.Context, ws uuid.UUID, kind *Kind) ([]*Document, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*Document
	for _, d := range f.stored {
		if d.WorkspaceID != ws {
			continue
		}
		if kind != nil && d.Kind != *kind {
			continue
		}
		c := *d
		out = append(out, &c)
	}
	return out, nil
}

func (f *fakeStore) Search(_ context.Context, ws uuid.UUID, _ []float32, kind *Kind, limit int) ([]*Document, error) {
	if f.err != nil {
		return nil, f.err
	}
	list, err := f.List(context.Background(), ws, kind)
	if err != nil {
		return nil, err
	}
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func (f *fakeStore) Delete(_ context.Context, ws, id uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	if d, ok := f.byID[id]; !ok || d.WorkspaceID != ws {
		return ErrNotFound
	}
	delete(f.byID, id)
	return nil
}

func TestNewValidation(t *testing.T) {
	ws := uuid.New()
	t.Run("valid document", func(t *testing.T) {
		d, err := New(ws, KindDocument, "Brand Guide", "Follow the tone.")
		require.NoError(t, err)
		require.Equal(t, ws, d.WorkspaceID)
		require.Equal(t, KindDocument, d.Kind)
	})
	t.Run("nil workspace rejected", func(t *testing.T) {
		_, err := New(uuid.Nil, KindDocument, "t", "c")
		require.ErrorIs(t, err, ErrWorkspaceMismatch)
	})
	t.Run("invalid kind rejected", func(t *testing.T) {
		_, err := New(ws, Kind("bogus"), "t", "c")
		require.ErrorIs(t, err, ErrInvalidKind)
	})
	t.Run("empty title rejected", func(t *testing.T) {
		_, err := New(ws, KindDocument, " ", "c")
		require.ErrorIs(t, err, ErrInvalidInput)
	})
	t.Run("oversized content rejected", func(t *testing.T) {
		big := make([]byte, maxContentLength+1)
		_, err := New(ws, KindDocument, "t", string(big))
		require.ErrorIs(t, err, ErrTooLarge)
	})
}

func TestServiceCreateStoresEmbeddedDocument(t *testing.T) {
	ctx := WithTrace(context.Background(), "trace-1", "span-1")
	ws := uuid.New()
	store := newFakeStore()
	eng := &fakeEmbedder{vec: []float32{0.5, 0.5, 0, 0, 0, 0, 0, 0, 0, 0}}
	svc := NewService(store, eng, nil, nil, 10)

	d, err := svc.Create(ctx, ws, KindStyleGuide, "Voice", "Use warm words.")
	require.NoError(t, err)
	require.Equal(t, ws, d.WorkspaceID)
	require.Len(t, d.Embedding, 10)
	require.Equal(t, 1, eng.calls)
	stored, err := store.Get(ctx, ws, d.ID)
	require.NoError(t, err)
	require.Equal(t, "Voice", stored.Title)
}

func TestServiceCreateRejectsInvalid(t *testing.T) {
	svc := NewService(newFakeStore(), &fakeEmbedder{}, nil, nil, 10)
	_, err := svc.Create(context.Background(), uuid.Nil, KindDocument, "t", "c")
	require.ErrorIs(t, err, ErrWorkspaceMismatch)
}

func TestServiceEmbedFailureUnsaved(t *testing.T) {
	ws := uuid.New()
	eng := &fakeEmbedder{err: errors.New("boom")}
	svc := NewService(newFakeStore(), eng, nil, nil, 10)
	_, err := svc.Create(context.Background(), ws, KindDocument, "t", "c")
	require.Error(t, err)
	require.Equal(t, 1, eng.calls)
}

func TestServiceGetTrustsStoreButVerifiesWorkspace(t *testing.T) {
	ws := uuid.New()
	store := newFakeStore()
	// A doc that belongs to a *different* workspace but is returned by the store
	// (malicious/buggy adapter): the service must still refuse it.
	other := uuid.New()
	d, _ := New(other, KindDocument, "T", "C")
	store.byID[d.ID] = d
	svc := NewService(store, &fakeEmbedder{}, nil, nil, 10)

	_, err := svc.Get(context.Background(), ws, d.ID)
	require.ErrorIs(t, err, ErrWorkspaceMismatch)

	got, err := svc.Get(context.Background(), other, d.ID)
	require.NoError(t, err)
	require.Equal(t, d.ID, got.ID)
}

func TestServiceSearchSelectsTopK(t *testing.T) {
	ws := uuid.New()
	store := newFakeStore()
	a, _ := New(ws, KindDocument, "A", "aa")
	b, _ := New(ws, KindDocument, "B", "bb")
	store.byID[a.ID] = a
	store.byID[b.ID] = b
	store.stored = []*Document{a, b}
	svc := NewService(store, &fakeEmbedder{}, nil, nil, 10)

	docs, err := svc.Search(context.Background(), ws, "query", nil, 1)
	require.NoError(t, err)
	require.Len(t, docs, 1)
}

func TestServiceDeleteMissingReportsNotFound(t *testing.T) {
	ws := uuid.New()
	store := newFakeStore()
	svc := NewService(store, &fakeEmbedder{}, nil, nil, 10)
	err := svc.Delete(context.Background(), ws, uuid.New())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestServiceAuditSinkReceivesCreateSuccessAndFailure(t *testing.T) {
	ws := uuid.New()
	var recs []AuditRecord
	sink := captureSink{fn: func(rec AuditRecord) { recs = append(recs, rec) }}
	svc := NewService(newFakeStore(), &fakeEmbedder{}, sink, nil, 10)
	_, err := svc.Create(context.Background(), ws, KindDocument, "T", "C")
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.Equal(t, "success", recs[0].Outcome)
	require.NotEmpty(t, recs[0].DocumentID)
}

type captureSink struct{ fn func(AuditRecord) }

func (c captureSink) Record(_ context.Context, rec AuditRecord) { c.fn(rec) }
