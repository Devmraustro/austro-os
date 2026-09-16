package team

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeDeptResolver struct {
	shouldFail bool
	failErr    error
}

func (f *fakeDeptResolver) Get(ctx context.Context, workspaceID, departmentID uuid.UUID) (uuid.UUID, error) {
	if f.shouldFail {
		return uuid.Nil, f.failErr
	}
	return workspaceID, nil
}

type fakeStore struct {
	created *Team
}

func (f *fakeStore) Create(ctx context.Context, t *Team) (*Team, error) {
	f.created = t
	return t, nil
}
func (f *fakeStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Team, error) {
	return &Team{ID: id, DepartmentID: uuid.New(), WorkspaceID: workspaceID, Name: "existing"}, nil
}
func (f *fakeStore) List(ctx context.Context, workspaceID uuid.UUID, departmentID *uuid.UUID) ([]*Team, error) {
	return nil, nil
}
func (f *fakeStore) ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error) {
	return Page{}, nil
}
func (f *fakeStore) Update(ctx context.Context, workspaceID uuid.UUID, t *Team) (*Team, error) {
	return t, nil
}
func (f *fakeStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	return nil
}

func TestTeamCreateRequiresDepartment(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, &fakeDeptResolver{shouldFail: false})
	ws := uuid.New()
	dept := uuid.New()
	_, err := svc.Create(context.Background(), ws, uuid.Nil, "team")
	require.Error(t, err, "team without department must be rejected")
	require.ErrorIs(t, err, ErrInvalidInput)

	_, err = svc.Create(context.Background(), uuid.Nil, dept, "team")
	require.Error(t, err, "team without workspace must be rejected")
}

func TestTeamCreateRejectsCrossWorkspaceParent(t *testing.T) {
	store := &fakeStore{}
	resolver := &fakeDeptResolver{shouldFail: true, failErr: ErrWorkspaceMismatch}
	svc := NewService(store, resolver)
	ws := uuid.New()
	dept := uuid.New()
	_, err := svc.Create(context.Background(), ws, dept, "team")
	require.Error(t, err, "cross-workspace department must be rejected")
	require.ErrorIs(t, err, ErrWorkspaceMismatch)
}

func TestTeamUpdateRejectsCrossWorkspaceMove(t *testing.T) {
	store := &fakeStore{}
	resolver := &fakeDeptResolver{shouldFail: true, failErr: ErrWorkspaceMismatch}
	svc := NewService(store, resolver)
	ws := uuid.New()
	teamID := uuid.New()
	newDept := uuid.New()
	_, err := svc.Update(context.Background(), ws, teamID, "new-name", &newDept)
	require.Error(t, err, "cross-workspace move must be rejected")
	require.ErrorIs(t, err, ErrWorkspaceMismatch)
}

func TestTeamCreateRejectsBlankName(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, &fakeDeptResolver{})
	ws := uuid.New()
	dept := uuid.New()
	_, err := svc.Create(context.Background(), ws, dept, "   ")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidInput)
}
