package ai

import (
	"context"
	"fmt"
	"strings"
)

// StubProvider is the deterministic, stateless backend used for tests and
// runtime wiring before a real provider is selected (ADR-003, ADR-004).
//
// All outputs are a pure function of the request inputs. It holds no shared
// state, so there is no mechanism by which data from one workspace can influence
// a later call from another workspace (Principle 10). It never contacts any
// external service, sends no paid model call, and stores no API key.
type StubProvider struct{}

// Complete returns a deterministic completion derived only from the request.
func (StubProvider) Complete(_ context.Context, req CompletionRequest) (CompletionResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return CompletionResult{}, err
	}
	if strings.TrimSpace(req.Instruction) == "" {
		return CompletionResult{}, fmt.Errorf("%w: instruction is empty", ErrInvalidInput)
	}
	// Grammar-deterministic output for verification: the completion echoes the
	// instruction length as a stable pseudo-token so callers can assert exact
	// reproducibility.
	return CompletionResult{Text: fmt.Sprintf("stub-completion[%d]", len([]rune(req.Instruction)))}, nil
}

// Embed returns a deterministic fixed vector derived only from the request.
func (StubProvider) Embed(_ context.Context, req EmbedRequest) (EmbeddingResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return EmbeddingResult{}, err
	}
	if strings.TrimSpace(req.Content) == "" {
		return EmbeddingResult{}, fmt.Errorf("%w: content is empty", ErrInvalidInput)
	}
	if req.Dimensions <= 0 {
		return EmbeddingResult{}, fmt.Errorf("%w: dimensions must be positive", ErrInvalidInput)
	}
	// A stable, dimension-consistent vector: first values collapse from a simple
	// hash, the rest are 0. The exact values are part of the stub contract and
	// must not change without a coordinated test update.
	values := make([]float32, req.Dimensions)
	sum := 0.0
	for _, r := range req.Content {
		sum += float64(r)
	}
	seed := float32(int64(sum)) + 1
	if len(values) > 0 {
		values[0] = seed
	}
	return EmbeddingResult{Values: values, Dimensions: req.Dimensions}, nil
}

// Classify returns a deterministic category picked from the request's
// categories using a stable tie-break on the input.
func (StubProvider) Classify(_ context.Context, req ClassificationRequest) (ClassificationResult, error) {
	if err := validateScope(req.Scope); err != nil {
		return ClassificationResult{}, err
	}
	if len(req.Categories) == 0 {
		return ClassificationResult{}, fmt.Errorf("%w: categories is empty", ErrInvalidInput)
	}
	if strings.TrimSpace(req.Input) == "" {
		return ClassificationResult{}, fmt.Errorf("%w: input is empty", ErrInvalidInput)
	}
	// Deterministic selection independent of map iteration order.
	sum := 0
	for _, r := range req.Input {
		sum += int(r)
	}
	idx := sum % len(req.Categories)
	return ClassificationResult{Category: req.Categories[idx]}, nil
}

func validateScope(s Scope) error {
	if strings.TrimSpace(s.WorkspaceID) == "" {
		return ErrWorkspaceRequired
	}
	return nil
}
