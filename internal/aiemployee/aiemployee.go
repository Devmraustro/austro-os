// Package aiemployee defines the AI Employee domain for the organizational hierarchy.
//
// AI Employee → exactly one Team (FK NOT NULL CASCADE)
// Team → exactly one Department
// Department → exactly one Workspace
// This package imports no infrastructure package.
package aiemployee

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput      = errors.New("aiemployee: invalid input")
	ErrNotFound          = errors.New("aiemployee: not found")
	ErrWorkspaceMismatch = errors.New("aiemployee: workspace mismatch")
	ErrNameTaken         = errors.New("aiemployee: name already exists")
)

// Role is the employee role (free-form but validated non-blank).
type Role string

// AIEmployee is the persistent aggregate.
type AIEmployee struct {
	ID              uuid.UUID  `json:"id"`
	TeamID          uuid.UUID  `json:"team_id"`
	DepartmentID    uuid.UUID  `json:"department_id"` // derived via team
	WorkspaceID     uuid.UUID  `json:"workspace_id"`  // derived via team->department
	Name            string     `json:"name"`
	Role            string     `json:"role"`
	Capabilities    []string   `json:"capabilities"`
	Permissions     any        `json:"permissions,omitempty"`
	MemoryID        *uuid.UUID `json:"memory_id,omitempty"`
	KnowledgeAccess any        `json:"knowledge_access,omitempty"`
	CurrentTaskID   *uuid.UUID `json:"current_task_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// New creates a valid employee.
func New(teamID, workspaceID, departmentID uuid.UUID, name, role string) (*AIEmployee, error) {
	if teamID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	if departmentID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	if name == "" || role == "" {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	return &AIEmployee{
		ID:           uuid.New(),
		TeamID:       teamID,
		DepartmentID: departmentID,
		WorkspaceID:  workspaceID,
		Name:         name,
		Role:         role,
		Capabilities: []string{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

type ListQuery struct {
	Limit  int
	TeamID *uuid.UUID
	Cursor Cursor
}

type Cursor struct {
	Set       bool
	CreatedAt time.Time
	ID        uuid.UUID
}

type Page struct {
	Employees  []*AIEmployee
	NextCursor string
	Limit      int
}

func (q *ListQuery) Normalize() {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
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

func EncodeCursor(e *AIEmployee) string {
	if e == nil {
		return ""
	}
	return e.ID.String()
}
