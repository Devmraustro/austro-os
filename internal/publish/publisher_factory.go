package publish

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Backend selectors understood by the publishing composition boundary. These
// are stable contract values shared with the strict configuration layer
// (internal/config): "stub" selects the offline deterministic adapter; any
// other value selects a real HTTP delivery backend that is CONFIGURATION_REQUIRED.
const (
	BackendStub        = "stub"
	BackendGenericHTTP = "generic-http"
)

// PublisherConfig selects and configures the Publisher adapter at the
// composition boundary. Selecting a non-stub backend requires WebhookURL and
// Token to be present and secure, or the factory fails fast.
type PublisherConfig struct {
	Backend string
	// WebhookURL is the external delivery endpoint (non-stub only).
	WebhookURL string
	// Token is the bearer credential for the delivery endpoint (non-stub only).
	// It is held in memory and never logged.
	Token string
	// HTTPClient is injected by tests (httptest.Server); nil uses a default
	// client with Timeout.
	HTTPClient *http.Client
	Timeout    time.Duration
}

// ErrPublisherConfig is returned when an unbuildable publisher is requested.
var ErrPublisherConfig = errors.New("publish: invalid publisher configuration")

// NewPublisher builds the Publisher selected by cfg. An empty Backend selects
// the deterministic StubPublisher. An unknown backend fails fast rather than
// silently degrading (fail-closed composition).
func NewPublisher(cfg PublisherConfig) (Publisher, error) {
	switch cfg.Backend {
	case "", BackendStub:
		return StubPublisher{}, nil
	case BackendGenericHTTP:
		return NewGenericHTTPPublisher(cfg)
	default:
		return nil, fmt.Errorf("%w: unknown backend %q", ErrPublisherConfig, cfg.Backend)
	}
}
