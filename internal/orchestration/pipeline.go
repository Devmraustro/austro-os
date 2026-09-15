// Package orchestration composes the Creator workflow: research → script →
// review → publish, as an event-driven, workspace-scoped pipeline (ROADMAP §5.4,
// ADR-010). It is a pure domain/application package with no infrastructure
// import. Stage work is delegated to narrow capability ports wired by composition.
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
	ErrUnauthorizedActor  = errors.New("orchestration: a verified actor is required")
	ErrConsecutiveAdvance = errors.New("orchestration: pipeline cannot advance without work")
	ErrRateLimited        = errors.New("orchestration: pipeline advance rate limit exceeded")
	ErrConcurrentUpdate   = errors.New("orchestration: pipeline changed concurrently")
)

// Stage is a single step of the Creator workflow. Stages advance in the fixed
// order research → script → review → publish, after which the pipeline is complete.
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
	StatusApproved         PipelineStatus = "approved"
	StatusDone             PipelineStatus = "done"
	StatusFailed           PipelineStatus = "failed"
)

// Pipeline is the Creator workflow aggregate, always scoped to a WorkspaceID.
// References are opaque, bounded stage outputs from the existing capability
// ports; this aggregate deliberately does not invent a content-studio model.
type Pipeline struct {
	ID                uuid.UUID      `json:"id"`
	WorkspaceID       uuid.UUID      `json:"workspace_id"`
	GoalID            *uuid.UUID     `json:"goal_id,omitempty"`
	Stage             Stage          `json:"stage"`
	Status            PipelineStatus `json:"status"`
	TaskID            *uuid.UUID     `json:"task_id,omitempty"`
	PublicationID     *uuid.UUID     `json:"publication_id,omitempty"`
	ResearchReference string         `json:"research_reference,omitempty"`
	ScriptReference   string         `json:"script_reference,omitempty"`
	ReviewReference   string         `json:"review_reference,omitempty"`
	PublishedReference string        `json:"published_reference,omitempty"`
	FailureReason     string         `json:"failure_reason,omitempty"`
	RetryCount        int            `json:"retry_count"`
	IdempotencyKey    string         `json:"-"`
	ApprovedBy        string         `json:"approved_by,omitempty"`
	ApprovedAt        *time.Time     `json:"approved_at,omitempty"`
	TraceID           string         `json:"trace_id,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	Version           int64          `json:"-"`
}

// New validates inputs and returns a freshly created pipeline.
func New(workspaceID uuid.UUID, goalID *uuid.UUID, traceID string) (*Pipeline, error) {
	return NewWithIdempotency(workspaceID, goalID, traceID, "")
}

// NewWithIdempotency creates a pipeline with a bounded, workspace-scoped
// request key. The key is not exposed in JSON responses.
func NewWithIdempotency(workspaceID uuid.UUID, goalID *uuid.UUID, traceID, idempotencyKey string) (*Pipeline, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(idempotencyKey) > 128 {
		return nil, ErrInvalidInput
	}
	now := time.Now().UTC()
	return &Pipeline{
		ID: uuid.New(), WorkspaceID: workspaceID, GoalID: goalID,
		Stage: StageResearch, Status: StatusCreated,
		TraceID: strings.TrimSpace(traceID), IdempotencyKey: idempotencyKey,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}, nil
}

// ValidStage reports whether s is recognized.
func ValidStage(s Stage) bool {
	switch s {
	case StageResearch, StageScript, StageReview, StagePublish, StageComplete:
		return true
	}
	return false
}

// ValidStatus reports whether s is recognized. It is exported for strict
// persistence and worker message validation.
func ValidStatus(s PipelineStatus) bool {
	switch s {
	case StatusCreated, StatusActive, StatusAwaitingApproval, StatusApproved, StatusDone, StatusFailed:
		return true
	}
	return false
}

func validStatus(s PipelineStatus) bool { return ValidStatus(s) }

func terminalStatus(s PipelineStatus) bool { return s == StatusDone }

// CanAdvance validates a server-authoritative transition. Failed pipelines are
// recovered only through Retry; approved review is the only legal input to
// publish.
func CanAdvance(stage Stage, from Stage, status PipelineStatus) error {
	if !ValidStage(stage) || !ValidStage(from) {
		return ErrInvalidStage
	}
	if !validStatus(status) {
		return ErrInvalidStatus
	}
	if terminalStatus(status) || from == StageComplete {
		return ErrTerminalState
	}
	if status == StatusFailed {
		return ErrTerminalState
	}
	switch from {
	case StageResearch:
		if stage != StageScript { return ErrInvalidTransition }
	case StageScript:
		if stage != StageReview { return ErrInvalidTransition }
	case StageReview:
		if stage != StagePublish { return ErrInvalidTransition }
		if status != StatusApproved { return ErrApprovalRequired }
	case StagePublish:
		if stage != StageComplete { return ErrInvalidTransition }
	default:
		return ErrInvalidStage
	}
	return nil
}

// IsAfter reports the fixed lifecycle ordering, used to make duplicate worker
// delivery idempotent without allowing a client to select a destination.
func IsAfter(a, b Stage) bool {
	order := map[Stage]int{StageResearch: 0, StageScript: 1, StageReview: 2, StagePublish: 3, StageComplete: 4}
	return order[a] > order[b]
}

// Approved reports whether a human approval has been durably recorded.
func (p *Pipeline) Approved() bool {
	return p != nil && p.Status == StatusApproved && p.ApprovedBy != "" && p.ApprovedAt != nil
}
