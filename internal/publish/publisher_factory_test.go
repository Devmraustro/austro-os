package publish

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewPublisherDefaultsToStub verifies an empty backend selects the offline
// deterministic adapter.
func TestNewPublisherDefaultsToStub(t *testing.T) {
	p, err := NewPublisher(PublisherConfig{})
	require.NoError(t, err)
	_, ok := p.(StubPublisher)
	require.True(t, ok, "expected StubPublisher")
}

// TestNewPublisherSelectsStub verifies the explicit stub selector.
func TestNewPublisherSelectsStub(t *testing.T) {
	p, err := NewPublisher(PublisherConfig{Backend: BackendStub})
	require.NoError(t, err)
	_, ok := p.(StubPublisher)
	require.True(t, ok)
}

// TestNewPublisherSelectsGenericHTTP verifies the real HTTP adapter is selected
// when fully configured.
func TestNewPublisherSelectsGenericHTTP(t *testing.T) {
	p, err := NewPublisher(PublisherConfig{
		Backend:    BackendGenericHTTP,
		WebhookURL: "https://hook.austro.internal/deliver",
		Token:      "tok-prod-1a2b3c4d5e6f",
	})
	require.NoError(t, err)
	_, ok := p.(*GenericHTTPPublisher)
	require.True(t, ok, "expected *GenericHTTPPublisher")
}

// TestNewPublisherRejectsMissingToken verifies the real adapter fails fast on
// absent credentials.
func TestNewPublisherRejectsMissingToken(t *testing.T) {
	_, err := NewPublisher(PublisherConfig{
		Backend:    BackendGenericHTTP,
		WebhookURL: "https://hook.austro.internal/deliver",
	})
	require.ErrorIs(t, err, ErrPublisherConfig)
}

// TestNewPublisherRejectsUnknownBackend verifies an unrecognized backend fails
// closed.
func TestNewPublisherRejectsUnknownBackend(t *testing.T) {
	_, err := NewPublisher(PublisherConfig{Backend: "mystery-http"})
	require.ErrorIs(t, err, ErrPublisherConfig)
}
