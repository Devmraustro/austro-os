package aiemployee

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeTeamResolver struct {
	shouldFail bool
	failErr    error
}

func (f *fakeTeamResolver) Get(ctx context.Context, workspaceID, teamID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	if f.shouldFail {
		return uuid.Nil, uuid.Nil, f.failErr
	}
	return workspaceID, uuid.New(), nil
}

type fakeStore struct {
	created *AIEmployee
}

func (f *fakeStore) Create(ctx context.Context, e *AIEmployee) (*AIEmployee, error) {
	f.created = e
	return e, nil
}
func (f *fakeStore) Get(ctx context.Context, workspaceID, id uuid.UUID) (*AIEmployee, error) {
	return &AIEmployee{ID: id, TeamID: uuid.New(), WorkspaceID: workspaceID, Name: "existing", Role: "r"}, nil
}
func (f *fakeStore) List(ctx context.Context, workspaceID uuid.UUID, teamID *uuid.UUID) ([]*AIEmployee, error) {
	return nil, nil
}
func (f *fakeStore) ListPage(ctx context.Context, workspaceID uuid.UUID, q ListQuery) (Page, error) {
	return Page{}, nil
}
func (f *fakeStore) Update(ctx context.Context, workspaceID uuid.UUID, e *AIEmployee) (*AIEmployee, error) {
	return e, nil
}
func (f *fakeStore) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	return nil
}

func TestAIEmployeeCreateRequiresTeam(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, &fakeTeamResolver{})
	ws := uuid.New()
	_, err := svc.Create(context.Background(), ws, uuid.Nil, "emp", "role", nil)
	require.Error(t, err, "employee without team must be rejected")
	require.ErrorIs(t, err, ErrInvalidInput)

	_, err = svc.Create(context.Background(), uuid.Nil, uuid.New(), "emp", "role", nil)
	require.Error(t, err)
}

func TestAIEmployeeCreateRejectsCrossWorkspaceTeam(t *testing.T) {
	store := &fakeStore{}
	resolver := &fakeTeamResolver{shouldFail: true, failErr: ErrWorkspaceMismatch}
	svc := NewService(store, resolver)
	ws := uuid.New()
	team := uuid.New()
	_, err := svc.Create(context.Background(), ws, team, "emp", "role", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrWorkspaceMismatch)
}

func TestAIEmployeeCreateRejectsBlankName(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, &fakeTeamResolver{})
	ws := uuid.New()
	team := uuid.New()
	_, err := svc.Create(context.Background(), ws, team, "   ", "role", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestAIEmployeeCreateRejectsBlankRole(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store, &fakeTeamResolver{})
	ws := uuid.New()
	team := uuid.New()
	_, err := svc.Create(context.Background(), ws, team, "name", "   ", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestAIEmployeeUpdateRejectsCrossWorkspaceMove(t *testing.T) {
	store := &fakeStore{}
	resolver := &fakeTeamResolver{shouldFail: true, failErr: ErrWorkspaceMismatch}
	svc := NewService(store, resolver)
	ws := uuid.New()
	empID := uuid.New()
	newTeam := uuid.New()
	_, err := svc.Update(context.Background(), ws, empID, nil, nil, nil, &newTeam, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrWorkspaceMismatch)
}
