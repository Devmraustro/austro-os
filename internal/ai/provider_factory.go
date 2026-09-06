package ai

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Backend selectors understood by the AI composition boundary. These are stable
// contract values shared with the strict configuration layer (internal/config):
// "stub" selects the offline deterministic adapter; "local" selects a keyless
// OpenAI-compatible endpoint for a local/free model server; any other value
// selects a real HTTP backend that is CONFIGURATION_REQUIRED.
const (
	BackendStub             = "stub"
	BackendLocal            = "local"
	BackendOpenAICompatible = "openai-compatible"
)

// ProviderConfig selects and configures the Provider adapter at the
// composition boundary. It mirrors the strict configuration model: selecting a
// non-stub backend requires Model, BaseURL and APIKey to be present and secure,
// or the factory fails fast (never silently degrades). The one exception is the
// "local" backend, whose APIKey is optional because a local model server often
// needs no credential.
type ProviderConfig struct {
	Backend string
	Model   string
	BaseURL string
	APIKey  string
	// HTTPClient is injected by tests (httptest.Server); nil uses a default
	// client with Timeout.
	HTTPClient *http.Client
	Timeout    time.Duration
}

// ErrProviderConfig is returned when an unbuildable provider is requested.
var ErrProviderConfig = errors.New("ai: invalid provider configuration")

// NewProvider builds the Provider selected by cfg. An empty Backend selects the
// deterministic StubProvider. An unknown backend fails fast rather than
// silently degrading to a different adapter (fail-closed composition).
func NewProvider(cfg ProviderConfig) (Provider, error) {
	switch cfg.Backend {
	case "", BackendStub:
		return StubProvider{}, nil
	case BackendLocal:
		return NewLocalProvider(cfg)
	case BackendOpenAICompatible:
		return NewOpenAICompatibleProvider(cfg)
	default:
		return nil, fmt.Errorf("%w: unknown backend %q", ErrProviderConfig, cfg.Backend)
	}
}
