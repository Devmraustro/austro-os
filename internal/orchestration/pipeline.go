// Package orchestration composes the Creator workflow: research → script →
// review → publish, as an event-driven, workspace-scoped pipeline (ROADMAP §5.4,
// ADR-010). It is a pure domain/application package with no infrastructure
// import (criterion #32). Stage work is delegated to narrow capability ports
// (P6 Replaceability) wired to deterministic stubs in Phase 2.
package orchestration

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels reported by the orchestration boundary.
var (
	ErrInvalidInput       = errors.New("orchestration: invalid input")
	ErrNotFound           = errors.New("orchestration: not found")
	ErrWorkspaceMismatch  = errors.New("orchestration: workspace mismatch")
	ErrInvalidTransition  = errors.New("orchestration: invalid stage transition")
	ErrTerminalState      = errors.New("orchestration: pipeline is in a terminal status")
	ErrInvalidStage       = errors.New("orchestration: invalid stage")
	ErrInvalidStatus      = errors.New("orchestration: invalid pipeline status")
	ErrApprovalRequired   = errors.New("orchestration: publication must be approved before publishing")
	ErrConsecutiveAdvance = errors.New("orchestration: pipeline cannot advance without work")
	ErrRateLimited        = errors.New("orchestration: pipeline advance rate limit exceeded")
)

// Stage is a single step of the Creator workflow. Stages advance in the fixed
// order research → script → review → publish, after which the pipeline is
// complete.
type Stage string

const (
	StageResearch Stage = "research"
	StageScript   Stage = "script"
	StageReview   Stage = "review"
	StagePublish  Stage = "publish"
	StageComplete Stage = "complete"
)

// PipelineStatus captures the lifecycle of a pipeline.
type PipelineStatus string

const (
	StatusCreated          PipelineStatus = "created"
	StatusActive           PipelineStatus = "active"
	StatusAwaitingApproval PipelineStatus = "awaiting_approval"
	StatusDone             PipelineStatus = "done"
	StatusFailed           PipelineStatus = "failed"
)

// Pipeline is the Creator workflow aggregate, always scoped to a WorkspaceID.
type Pipeline struct {
	ID            uuid.UUID      `json:"id"`
	WorkspaceID   uuid.UUID      `json:"workspace_id"`
	GoalID        *uuid.UUID     `json:"goal_id,omitempty"`
	Stage         Stage          `json:"stage"`
	Status        PipelineStatus `json:"status"`
	TaskID        *uuid.UUID     `json:"task_id,omitempty"`
	PublicationID *uuid.UUID     `json:"publication_id,omitempty"`
	TraceID       string         `json:"trace_id,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// New validates inputs and returns a freshly created (research, created)
// pipeline as a value aggregate (not yet persisted).
func New(workspaceID uuid.UUID, goalID *uuid.UUID, traceID string) (*Pipeline, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	now := time.Now().UTC()
	return &Pipeline{
		ID:          uuid.New(),
		WorkspaceID: workspaceID,
		GoalID:      goalID,
		Stage:       StageResearch,
		Status:      StatusCreated,
		TraceID:     strings.TrimSpace(traceID),
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// ValidStage reports whether s is a recognized pipeline stage.
func ValidStage(s Stage) bool {
	switch s {
	case StageResearch, StageScript, StageReview, StagePublish, StageComplete:
		return true
	}
	return false
}

// validStatus reports whether s is a recognized pipeline status.
func validStatus(s PipelineStatus) bool {
	switch s {
	case StatusCreated, StatusActive, StatusAwaitingApproval, StatusDone, StatusFailed:
		return true
	}
	return false
}

// terminalStatus reports whether the status is final (no further transitions).
func terminalStatus(s PipelineStatus) bool {
	return s == StatusDone || s == StatusFailed
}

// CanAdvance validates a stage transition on a pipeline whose status is `from`.
// A stage is only advanced to the next stage in the fixed order; terminal
// statuses cannot advance.
func CanAdvance(stage Stage, from Stage, status PipelineStatus) error {
	if !ValidStage(stage) || !ValidStage(from) {
		return ErrInvalidStage
	}
	if !validStatus(status) {
		return ErrInvalidStatus
	}
	if terminalStatus(status) {
		return ErrTerminalState
	}
	switch from {
	case StageResearch:
		if stage != StageScript {
			return ErrInvalidTransition
		}
	case StageScript:
		if stage != StageReview {
			return ErrInvalidTransition
		}
	case StageReview:
		if stage != StagePublish {
			return ErrInvalidTransition
		}
	case StagePublish:
		if stage != StageComplete {
			return ErrInvalidTransition
		}
	default:
		return ErrInvalidStage
	}
	return nil
}
