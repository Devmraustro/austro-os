package department

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	created *Department
}

func (f *fakeStore) Create(ctx context.Context, d *Department) (*Department, error) {
	f.created = d
	return d, nil
}
func (f *fakeStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Department, error) {
	return &Department{ID: id, WorkspaceID: workspaceID, Name: "existing"}, nil
}
func (f *fakeStore) List(ctx context.Context, workspaceID uuid.UUID) ([]*Department, error) {
	return nil, nil
}
func (f *fakeStore) ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error) {
	return Page{}, nil
}
func (f *fakeStore) Update(ctx context.Context, workspaceID uuid.UUID, d *Department) (*Department, error) {
	return d, nil
}
func (f *fakeStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	return nil
}

func TestDepartmentCreateRequiresName(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store)
	ws := uuid.New()
	_, err := svc.Create(context.Background(), ws, "   ")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestDepartmentCreateRequiresWorkspace(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store)
	_, err := svc.Create(context.Background(), uuid.Nil, "dept")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestDepartmentUpdateRequiresName(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store)
	ws := uuid.New()
	id := uuid.New()
	_, err := svc.Update(context.Background(), ws, id, "   ")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidInput)
}
