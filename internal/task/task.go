// Package task provides the Task Management domain for the Creator pipeline.
//
// Tasks are the unit of work assigned to bounded execution identities (AI
// employees) under ADR-002. They are workspace-scoped, explicit in their
// lifecycle, deny-by-default in authorization, auditable, and traceable. The
// package defines the Task model, a workspace-scoped persistence port
// (TaskStore), and a Service that enforces workspace ownership and lifecycle
// transitions while emitting audit events and structured logs. It imports no
// infrastructure package (Phase 1 criterion #32 preserves dependency
// direction).
package task

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Sentinels reported by the task boundary.
var (
	ErrInvalidInput       = errors.New("task: invalid input")
	ErrNotFound           = errors.New("task: not found")
	ErrWorkspaceMismatch  = errors.New("task: workspace mismatch")
	ErrInvalidTransition  = errors.New("task: invalid status transition")
	ErrTerminalState      = errors.New("task: task is in a terminal state")
	ErrPriority           = errors.New("task: invalid priority")
	ErrStatus             = errors.New("task: invalid status")
	ErrAssigneeType       = errors.New("task: invalid assignee type")
)

// Status is a task lifecycle stage. Statuses are stored as text (schema is
// additive) and validated here.
type Status string

const (
	StatusBacklog   Status = "backlog"
	StatusPlanned   Status = "planned"
	StatusInProgress Status = "in_progress"
	StatusInReview  Status = "in_review"
	StatusCompleted Status = "completed"
	StatusCancelled Status = "cancelled"
	StatusRejected  Status = "rejected"
	StatusFailed    Status = "failed"
)

// Priority is a task scheduling priority.
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

// AssigneeType distinguishes AI-employee vs human assignment.
type AssigneeType string

const (
	AssigneeAI    AssigneeType = "ai_employee"
	AssigneeHuman AssigneeType = "human"
)

// Task is the persistent task aggregate. It is always scoped to a WorkspaceID.
type Task struct {
	ID           uuid.UUID    `json:"id"`
	WorkspaceID  uuid.UUID    `json:"workspace_id"`
	GoalID       *uuid.UUID   `json:"goal_id,omitempty"`
	ParentTaskID *uuid.UUID   `json:"parent_task_id,omitempty"`
	Title        string       `json:"title"`
	Description  string       `json:"description"`
	Status       Status       `json:"status"`
	Priority     Priority     `json:"priority"`
	AssigneeType AssigneeType `json:"assignee_type,omitempty"`
	AssigneeID   *uuid.UUID   `json:"assignee_id,omitempty"`
	Deadline     *time.Time   `json:"deadline,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// New creates a valid task record from validated inputs. WorkspaceID is
// required; title must be non-empty. The initial status is backlog.
func New(workspaceID uuid.UUID, title string, p Priority, description string) (*Task, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	if title == "" {
		return nil, ErrInvalidInput
	}
	if !ValidPriority(p) {
		return nil, ErrPriority
	}
	now := time.Now().UTC()
	return &Task{
		ID:          uuid.New(),
		WorkspaceID: workspaceID,
		Title:       title,
		Description: description,
		Status:      StatusBacklog,
		Priority:    p,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// ValidStatus reports whether s is a recognized lifecycle status.
func ValidStatus(s Status) bool {
	switch s {
	case StatusBacklog, StatusPlanned, StatusInProgress, StatusInReview,
		StatusCompleted, StatusCancelled, StatusRejected, StatusFailed:
		return true
	}
	return false
}

// ValidPriority reports whether p is a recognized priority.
func ValidPriority(p Priority) bool {
	switch p {
	case PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent:
		return true
	}
	return false
}

// ValidAssigneeType reports whether t is a recognized assignee type.
func ValidAssigneeType(t AssigneeType) bool {
	switch t {
	case AssigneeAI, AssigneeHuman:
		return true
	}
	return false
}

// terminal reports whether the status is final (no outgoing transitions).
func terminal(s Status) bool {
	switch s {
	case StatusCompleted, StatusCancelled, StatusFailed, StatusRejected:
		return true
	}
	return false
}

// allowedTransition reports whether moving from -> to is permitted by the
// explicit lifecycle.
func allowedTransition(from, to Status) bool {
	switch from {
	case StatusBacklog:
		return to == StatusPlanned || to == StatusCancelled || to == StatusFailed
	case StatusPlanned:
		return to == StatusInProgress || to == StatusCancelled || to == StatusFailed
	case StatusInProgress:
		return to == StatusInReview || to == StatusCancelled || to == StatusFailed
	case StatusInReview:
		return to == StatusCompleted || to == StatusRejected || to == StatusCancelled || to == StatusFailed
	}
	return false
}

// CanTransition validates a status change on a task.
func CanTransition(from, to Status) error {
	if !ValidStatus(from) || !ValidStatus(to) {
		return ErrStatus
	}
	if terminal(from) {
		return ErrTerminalState
	}
	if from == to {
		return ErrInvalidTransition
	}
	if !allowedTransition(from, to) {
		return ErrInvalidTransition
	}
	return nil
}