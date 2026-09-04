package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GenericHTTPPublisher is a real (CONFIGURATION_REQUIRED) Publisher that
// delivers an approved publication to a generic HTTP endpoint. It implements
// the Publisher port so callers never depend on it directly (Replaceability,
// Principle 6).
//
// Consistent with StubPublisher and the Human Oversight gate, it refuses any
// publication that is not approved. The bearer token is held in memory and only
// attached to the Authorization header; it is never logged or returned.
type GenericHTTPPublisher struct {
	webhookURL string
	token      string
	client     *http.Client
}

// NewGenericHTTPPublisher validates the CONFIGURATION_REQUIRED settings and
// returns the HTTP-backed publisher. It fails fast on any missing setting.
func NewGenericHTTPPublisher(cfg PublisherConfig) (*GenericHTTPPublisher, error) {
	if strings.TrimSpace(cfg.WebhookURL) == "" {
		return nil, fmt.Errorf("%w: webhook url is required", ErrPublisherConfig)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("%w: token is required", ErrPublisherConfig)
	}
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	return &GenericHTTPPublisher{
		webhookURL: cfg.WebhookURL,
		token:      cfg.Token,
		client:     client,
	}, nil
}

// Publish delivers an approved publication to the webhook and returns the
// external reference echoed by the endpoint (falling back to a stable
// deterministic reference when the endpoint returns none).
func (p *GenericHTTPPublisher) Publish(ctx context.Context, pub *Publication) (string, error) {
	if pub == nil || !pub.HasApproval() {
		return "", ErrApprovalRequired
	}
	body, err := json.Marshal(pub)
	if err != nil {
		return "", fmt.Errorf("publish: encode publication: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.webhookURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("publish: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("publish: delivery request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("publish: read delivery response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Never surface the token; report only the status.
		return "", fmt.Errorf("publish: delivery endpoint returned status %d", resp.StatusCode)
	}

	var out struct {
		ExternalID string `json:"external_id"`
		Reference  string `json:"reference"`
	}
	_ = json.Unmarshal(raw, &out)
	if strings.TrimSpace(out.ExternalID) != "" {
		return out.ExternalID, nil
	}
	if strings.TrimSpace(out.Reference) != "" {
		return out.Reference, nil
	}
	// Deterministic fallback reference derived only from the publication.
	return "generic-http://" + pub.ContentHash, nil
}

// ensure compile-time satisfaction of the Publisher port.
var _ Publisher = (*GenericHTTPPublisher)(nil)
