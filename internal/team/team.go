// Package team defines the Team domain for the organizational hierarchy.
//
// Teams are workspace-scoped via their department: Team → exactly one Department (FK NOT NULL CASCADE).
// Department → exactly one Workspace.
// This package imports no infrastructure package.
package team

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput      = errors.New("team: invalid input")
	ErrNotFound          = errors.New("team: not found")
	ErrWorkspaceMismatch = errors.New("team: workspace mismatch")
	ErrNameTaken         = errors.New("team: name already exists")
)

// Team is the persistent team aggregate.
type Team struct {
	ID           uuid.UUID `json:"id"`
	DepartmentID uuid.UUID `json:"department_id"`
	WorkspaceID  uuid.UUID `json:"workspace_id"` // derived via department, for API convenience and scoping
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// New creates a valid team record.
func New(departmentID, workspaceID uuid.UUID, name string) (*Team, error) {
	if departmentID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	if name == "" {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	return &Team{
		ID:           uuid.New(),
		DepartmentID: departmentID,
		WorkspaceID:  workspaceID,
		Name:         name,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// ListQuery for teams.
type ListQuery struct {
	Limit        int
	DepartmentID *uuid.UUID
	Cursor       Cursor
}

type Cursor struct {
	Set       bool
	CreatedAt time.Time
	ID        uuid.UUID
}

type Page struct {
	Teams      []*Team
	NextCursor string
	Limit      int
}

func (q *ListQuery) Normalize() {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 100 {
		q.Limit = 100
	}
}

func DecodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return Cursor{}, ErrInvalidInput
	}
	return Cursor{Set: true, ID: id}, nil
}

func EncodeCursor(t *Team) string {
	if t == nil {
		return ""
	}
	return t.ID.String()
}
