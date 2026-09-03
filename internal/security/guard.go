package security

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Guard errors: fail-closed preconditions for deny-by-default (ADR-011 §9).
var (
	ErrMissingWorkspace = errors.New("security: workspace is required (deny-by-default)")
	ErrMissingActor     = errors.New("security: a human actor is required (deny-by-default)")
)

// Guard consolidates the deny-by-default preconditions shared by Phase 2
// services. A Guard's checks fail closed: an invalid or empty workspace, or an
// empty human actor, is rejected rather than defaulted.
type Guard struct{}

// RequireWorkspace returns an error unless id is a non-nil, non-empty UUID. It
// prevents cross-boundary operations from proceeding with a missing workspace
// context.
func (Guard) RequireWorkspace(id uuid.UUID) error {
	if id == uuid.Nil {
		return ErrMissingWorkspace
	}
	return nil
}

// RequireHuman returns an error unless actor is a non-blank human identifier.
// Used before approval/rejection/publish transitions to enforce the Human
// Oversight gate.
func (Guard) RequireHuman(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return ErrMissingActor
	}
	return nil
}
