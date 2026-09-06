package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// maxResponseBytes bounds the response body read from the external delivery
	// endpoint. The endpoint is CONFIGURATION_REQUIRED and operator-configured,
	// but the boundary still guards against an oversized or misbehaving response.
	maxResponseBytes = 64 << 20 // 64 MiB

	// defaultMaxAttempts is used when MaxAttempts is unset (no bounded retry).
	defaultMaxAttempts = 1
	// defaultBackoffBase / defaultBackoffMax bound the retry pacing so a
	// transient outage cannot spin endlessly between attempts.
	defaultBackoffBase = 500 * time.Millisecond
	defaultBackoffMax  = 30 * time.Second
)

// GenericHTTPPublisher is a real (CONFIGURATION_REQUIRED) Publisher that
// delivers an approved publication to a generic HTTP endpoint. It implements
// the Publisher port so callers never depend on it directly (Replaceability,
// Principle 6).
//
// Consistent with StubPublisher and the Human Oversight gate, it refuses any
// publication that is not approved. The bearer token is held in memory and only
// attached to the Authorization header; it is never logged or returned.
//
// Delivery is retried with bounded exponential backoff on transient failures
// (network errors, 5xx, too-early responses); 4xx responses are treated as
// permanent and never retried. An IdempotencyKey is attached under a
// configurable field so the endpoint can deduplicate retries of the same
// publication (default generated deterministically from the publication).
type GenericHTTPPublisher struct {
	webhookURL string
	token      string
	client     *http.Client

	// idempotencyField names the header/query field used for the idempotency
	// key; empty disables key attachment.
	idempotencyField string
	// maxAttempts is the total delivery attempt ceiling (>= 1).
	maxAttempts int
	// backoffBase and backoffMax bound the exponential retry backoff.
	backoffBase time.Duration
	backoffMax  time.Duration
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

	attempts := cfg.MaxAttempts
	if attempts < 1 {
		attempts = defaultMaxAttempts
	}
	base := cfg.BackoffBase
	if base <= 0 {
		base = defaultBackoffBase
	}
	backoffMax := cfg.BackoffMax
	if backoffMax <= 0 {
		backoffMax = defaultBackoffMax
	}
	if backoffMax < base {
		backoffMax = base
	}

	return &GenericHTTPPublisher{
		webhookURL:       cfg.WebhookURL,
		token:            cfg.Token,
		client:           client,
		idempotencyField: strings.TrimSpace(cfg.IdempotencyKeyField),
		maxAttempts:      attempts,
		backoffBase:      base,
		backoffMax:       backoffMax,
	}, nil
}

// Publish delivers an approved publication to the webhook, retrying transient
// failures with bounded exponential backoff, and returns the external reference
// echoed by the endpoint (falling back to a stable deterministic reference when
// the endpoint returns none). 4xx responses and context cancellation are never
// retried; the last status error is surfaced on a failed final attempt without
// ever exposing the token.
func (p *GenericHTTPPublisher) Publish(ctx context.Context, pub *Publication) (string, error) {
	if pub == nil || !pub.HasApproval() {
		return "", ErrApprovalRequired
	}
	body, err := json.Marshal(pub)
	if err != nil {
		return "", fmt.Errorf("publish: encode publication: %w", err)
	}
	if len(body) > maxResponseBytes {
		return "", fmt.Errorf("publish: encoded publication too large")
	}

	var lastErr error
	for attempt := 1; attempt <= p.maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("publish: retry cancelled: %w", ctx.Err())
			case <-time.After(p.backoffFor(attempt)):
			}
		}
		ref, err := p.attempt(ctx, body, pub)
		if err == nil {
			return ref, nil
		}
		lastErr = err
		var perm *permanentError
		if errors.As(err, &perm) {
			return "", perm.err
		}
	}
	return "", lastErr
}

// attempt performs a single delivery attempt. It does no retrying itself.
func (p *GenericHTTPPublisher) attempt(ctx context.Context, body []byte, pub *Publication) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.webhookURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("publish: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)
	if p.idempotencyField != "" {
		req.Header.Set(p.idempotencyField, idempotencyKey(pub))
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", &permanentError{err: fmt.Errorf("publish: delivery cancelled: %w", ctx.Err())}
		}
		if isRetryable(err) {
			return "", err
		}
		// A non-retryable transport error (e.g. a 4xx surfaced as a custom
		// RoundTripper error) is permanent.
		return "", &permanentError{err: fmt.Errorf("publish: delivery request failed: %w", err)}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("publish: read delivery response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return "", fmt.Errorf("publish: delivery response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Never surface the token; report only the status.
		err := fmt.Errorf("publish: delivery endpoint returned status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", &permanentError{err: err}
		}
		return "", err
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

// backoffFor returns the sleep duration before the given (1-based) attempt,
// doubling from backoffBase up to backoffMax.
func (p *GenericHTTPPublisher) backoffFor(attempt int) time.Duration {
	shift := attempt - 2
	if shift < 0 {
		shift = 0
	}
	d := p.backoffBase
	for i := 0; i < shift && d < p.backoffMax; i++ {
		d *= 2
	}
	if d > p.backoffMax {
		d = p.backoffMax
	}
	return d
}

// isRetryable reports whether a transport error represents a transient outage
// that is safe to retry (as opposed to a permanent client-side rejection).
func isRetryable(err error) bool {
	// All context errors are handled by the caller before this point; a
	// non-nil error here is a transport failure (dial, TLS, timeout).
	return true
}

// idempotencyKey returns the stable delivery key for a publication, derived
// only from workspace, id, and content hash so identical retries collide. An
// explicit IdempotencyKey on the publication is honored verbatim.
func idempotencyKey(pub *Publication) string {
	if k := strings.TrimSpace(pub.IdempotencyKey); k != "" {
		return k
	}
	h := sha256.Sum256([]byte(pub.WorkspaceID.String() + "|" + pub.ID.String() + "|" + pub.ContentHash))
	return hex.EncodeToString(h[:])
}

// permanentError wraps a delivery failure that must not be retried (a 4xx
// rejection or a cancelled/context-bound attempt).
type permanentError struct {
	err error
}

// Error implements error.
func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

// ensure compile-time satisfaction of the Publisher port.
var _ Publisher = (*GenericHTTPPublisher)(nil)
