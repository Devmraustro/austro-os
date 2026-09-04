package ai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewProviderDefaultsToStub verifies an empty backend selects the offline
// deterministic adapter.
func TestNewProviderDefaultsToStub(t *testing.T) {
	p, err := NewProvider(ProviderConfig{})
	require.NoError(t, err)
	_, ok := p.(StubProvider)
	require.True(t, ok, "expected StubProvider")
}

// TestNewProviderSelectsStub verifies the explicit stub selector.
func TestNewProviderSelectsStub(t *testing.T) {
	p, err := NewProvider(ProviderConfig{Backend: BackendStub})
	require.NoError(t, err)
	_, ok := p.(StubProvider)
	require.True(t, ok)
}

// TestNewProviderSelectsOpenAICompatible verifies the real HTTP adapter is
// selected when fully configured.
func TestNewProviderSelectsOpenAICompatible(t *testing.T) {
	p, err := NewProvider(ProviderConfig{
		Backend: BackendOpenAICompatible,
		Model:   "gpt-test",
		BaseURL: "https://ai.austro.internal/v1",
		APIKey:  "sk-prod-9f8e7d6c5b4a",
	})
	require.NoError(t, err)
	_, ok := p.(*OpenAICompatibleProvider)
	require.True(t, ok, "expected *OpenAICompatibleProvider")
}

// TestNewProviderRejectsMissingCredential verifies the real adapter fails fast
// on absent credentials rather than constructing a provider that fails mid-call.
func TestNewProviderRejectsMissingCredential(t *testing.T) {
	_, err := NewProvider(ProviderConfig{
		Backend: BackendOpenAICompatible,
		Model:   "gpt-test",
		BaseURL: "https://ai.austro.internal/v1",
	})
	require.ErrorIs(t, err, ErrProviderConfig)
}

// TestNewProviderRejectsUnknownBackend verifies an unrecognized backend fails
// closed instead of silently degrading to the stub.
func TestNewProviderRejectsUnknownBackend(t *testing.T) {
	_, err := NewProvider(ProviderConfig{Backend: "mystery-model"})
	require.ErrorIs(t, err, ErrProviderConfig)
}
