// Package publish provides the Publishing + Approvals domain for the Creator
// pipeline.
//
// Publications follow an explicit, reviewed state machine with human approval
// mandatory before any external publish (Human Oversight, P11): queued → review
// → approved → published, with a valid rejection path (review → rejected) and a
// cancelled terminal exit. Platform delivery happens behind a replaceable
// Publisher port (Replaceability, P6); the first adapter is a deterministic
// stub. This package defines the Publication model, the workspace-scoped
// PublicationStore and Publisher ports, and a Service that enforces workspace
// ownership, the explicit lifecycle, human approval, rate limiting, and
// principle-tagged audit/structured logs. It imports no infrastructure package
// (Phase 1 criterion #32 preserves dependency direction).
package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinels reported by the publishing boundary.
var (
	ErrInvalidInput          = errors.New("publish: invalid input")
	ErrNotFound              = errors.New("publish: not found")
	ErrWorkspaceMismatch     = errors.New("publish: workspace mismatch")
	ErrInvalidTransition     = errors.New("publish: invalid status transition")
	ErrTerminalState         = errors.New("publish: publication is in a terminal state")
	ErrInvalidStatus         = errors.New("publish: invalid status")
	ErrApprovalRequired      = errors.New("publish: publication must be approved before publishing")
	ErrUnauthorizedApprover  = errors.New("publish: only a human may approve")
	ErrUnauthorizedRejecter  = errors.New("publish: only a human may reject")
	ErrUnauthorizedPublisher = errors.New("publish: only a human may publish")
	ErrRateLimited           = errors.New("publish: publishing rate limit exceeded")
	ErrEmptyContent          = errors.New("publish: title or body is empty")
)

// Status is a publication lifecycle stage. Statuses are stored as text (schema
// is additive) and validated here.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusReview    Status = "review"
	StatusApproved  Status = "approved"
	StatusPublished Status = "published"
	// StatusFailed records a durable delivery failure. It is terminal until a
	// future retry command is explicitly added; it is never reported as success.
	StatusFailed    Status = "failed"
	StatusRejected  Status = "rejected"
	StatusCancelled Status = "cancelled"
)

// StubPlatform is the only adapter in Phase 2 (ADR-009, ROADMAP §5.10).
const StubPlatform = "stub"

// Publication is the persistent publication aggregate. It is always scoped to a
// WorkspaceID and binds a deterministic ContentHash to the published body.
type Publication struct {
	ID          uuid.UUID  `json:"id"`
	WorkspaceID uuid.UUID  `json:"workspace_id"`
	GoalID      *uuid.UUID `json:"goal_id,omitempty"`
	TaskID      *uuid.UUID `json:"task_id,omitempty"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Platform    string     `json:"platform"`
	Status      Status     `json:"status"`
	ContentHash string     `json:"content_hash"`
	// IdempotencyKey is an optional stable delivery key for the external
	// endpoint. When empty, a deterministic key derived from workspace|id|hash
	// is used so retries of the same publication collide.
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	ApprovedBy     *string    `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time `json:"approved_at,omitempty"`
	RejectedBy     *string    `json:"rejected_by,omitempty"`
	RejectedAt     *time.Time `json:"rejected_at,omitempty"`
	PublishedAt    *time.Time `json:"published_at,omitempty"`
	PublishedBy    *string    `json:"published_by,omitempty"`
	ExternalReference string  `json:"external_reference,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// New validates inputs and returns a queued publication as a value aggregate
// (not yet persisted). WorkspaceID must be non-empty; title/body non-empty.
func New(workspaceID uuid.UUID, goalID, taskID *uuid.UUID, title, body, platform string) (*Publication, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceMismatch
	}
	if strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" {
		return nil, ErrEmptyContent
	}
	if strings.TrimSpace(platform) == "" {
		platform = StubPlatform
	}
	now := time.Now().UTC()
	d := ContentDigest(title, body)
	return &Publication{
		ID:          uuid.New(),
		WorkspaceID: workspaceID,
		GoalID:      goalID,
		TaskID:      taskID,
		Title:       strings.TrimSpace(title),
		Body:        body,
		Platform:    strings.TrimSpace(platform),
		Status:      StatusQueued,
		ContentHash: d,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// ContentDigest returns the deterministic SHA-256 digest of a publication's
// exact title+body, bound to the stub's published record (idempotency).
func ContentDigest(title, body string) string {
	h := sha256.Sum256([]byte(title + "\x00" + body))
	return hex.EncodeToString(h[:])
}

// ValidStatus reports whether s is a recognized lifecycle status.
func ValidStatus(s Status) bool {
	switch s {
	case StatusQueued, StatusReview, StatusApproved, StatusPublished, StatusFailed,
		StatusRejected, StatusCancelled:
		return true
	}
	return false
}

// terminal reports whether the status is final (no outgoing transitions).
func terminal(s Status) bool {
	switch s {
	case StatusPublished, StatusFailed, StatusRejected, StatusCancelled:
		return true
	}
	return false
}

// allowedTransition reports whether moving from -> to is permitted by the
// explicit lifecycle.
func allowedTransition(from, to Status) bool {
	switch from {
	case StatusQueued:
		return to == StatusReview || to == StatusCancelled
	case StatusReview:
		return to == StatusApproved || to == StatusRejected || to == StatusCancelled
	case StatusApproved:
		return to == StatusPublished || to == StatusFailed
	case StatusFailed:
		// Only the explicit Retry command may recover a failed delivery.
		return to == StatusPublished
	}
	return false
}

// CanTransition validates a status change on a publication.
func CanTransition(from, to Status) error {
	if !ValidStatus(from) || !ValidStatus(to) {
		return ErrInvalidStatus
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
