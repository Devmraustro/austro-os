package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// approvedPublication returns a publication that carries a recorded human
// approval marker (Human Oversight gate satisfied) for delivery tests.
func approvedPublication(t *testing.T) *Publication {
	t.Helper()
	ws := uuid.New()
	now := time.Now().UTC()
	p, err := New(ws, nil, nil, "Title", "Body", "generic-http")
	require.NoError(t, err)
	p.Status = StatusApproved
	who := "alice"
	p.ApprovedBy = &who
	p.ApprovedAt = &now
	return p
}

// TestGenericHTTPPublisherPublishesApproved verifies Publish sends an
// authenticated request and returns the external reference from the endpoint.
func TestGenericHTTPPublisherPublishesApproved(t *testing.T) {
	var gotKey string
	var gotBody map[string]any
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotKey = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"external_id":"vid-123"}`))
	}))
	defer srv.Close()

	pub := approvedPublication(t)
	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:    BackendGenericHTTP,
		WebhookURL: srv.URL,
		Token:      "tok-prod-1a2b3c4d5e6f",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	ref, err := httpPub.Publish(context.Background(), pub)
	require.NoError(t, err)
	require.Equal(t, "vid-123", ref)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "Bearer tok-prod-1a2b3c4d5e6f", gotKey)
	require.Equal(t, "Title", gotBody["title"])
	require.Equal(t, "generic-http", gotBody["platform"])
}

// TestGenericHTTPPublisherRejectsUnapproved verifies an unapproved publication
// is refused before any outbound call (second Human Oversight guard).
func TestGenericHTTPPublisherRejectsUnapproved(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:    BackendGenericHTTP,
		WebhookURL: srv.URL,
		Token:      "tok-prod-1a2b3c4d5e6f",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	ws := uuid.New()
	pub, err := New(ws, nil, nil, "Title", "Body", "generic-http")
	require.NoError(t, err)

	_, err = httpPub.Publish(context.Background(), pub)
	require.ErrorIs(t, err, ErrApprovalRequired)
	require.False(t, called, "no outbound call must be made for an unapproved publication")
}

// TestGenericHTTPPublisherErrorNeverLeaksToken verifies a delivery failure is
// surfaced without echoing the bearer token.
func TestGenericHTTPPublisherErrorNeverLeaksToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:    BackendGenericHTTP,
		WebhookURL: srv.URL,
		Token:      "tok-prod-1a2b3c4d5e6f",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, err = httpPub.Publish(context.Background(), approvedPublication(t))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "tok-prod")
	require.Contains(t, err.Error(), "403")
}

// TestGenericHTTPPublisherFallbackRef verifies a deterministic reference is
// returned when the endpoint returns no external id.
func TestGenericHTTPPublisherFallbackRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:    BackendGenericHTTP,
		WebhookURL: srv.URL,
		Token:      "tok-prod-1a2b3c4d5e6f",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	pub := approvedPublication(t)
	ref, err := httpPub.Publish(context.Background(), pub)
	require.NoError(t, err)
	require.Equal(t, "generic-http://"+pub.ContentHash, ref)
}

// TestGenericHTTPPublisherOversizedResponseRejected verifies an oversized
// delivery response fails the publish instead of buffering unbounded memory.
func TestGenericHTTPPublisherOversizedResponseRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxResponseBytes+1))
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:    BackendGenericHTTP,
		WebhookURL: srv.URL,
		Token:      "tok-prod-1a2b3c4d5e6f",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, err = httpPub.Publish(context.Background(), approvedPublication(t))
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "too large")
}

// TestGenericHTTPPublisherRetriesTransient5xx verifies a transient 5xx is
// retried (bounded) and eventually succeeds; 4xx is never retried.
func TestGenericHTTPPublisherRetriesTransient5xx(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		c := calls
		mu.Unlock()
		if c < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"external_id":"vid-retry"}`))
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:     BackendGenericHTTP,
		WebhookURL:  srv.URL,
		Token:       "tok-prod-1a2b3c4d5e6f",
		HTTPClient:  srv.Client(),
		MaxAttempts: 4,
		BackoffBase: 5 * time.Millisecond,
		BackoffMax:  20 * time.Millisecond,
	})
	require.NoError(t, err)

	ref, err := httpPub.Publish(context.Background(), approvedPublication(t))
	require.NoError(t, err)
	require.Equal(t, "vid-retry", ref)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 3, calls)
}

// TestGenericHTTPPublisherNeverRetries4xx verifies a client-side rejection
// returns immediately without further attempts.
func TestGenericHTTPPublisherNeverRetries4xx(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:     BackendGenericHTTP,
		WebhookURL:  srv.URL,
		Token:       "tok-prod-1a2b3c4d5e6f",
		HTTPClient:  srv.Client(),
		MaxAttempts: 5,
		BackoffBase: time.Millisecond,
	})
	require.NoError(t, err)

	_, err = httpPub.Publish(context.Background(), approvedPublication(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "422")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, calls)
}

// TestGenericHTTPPublisherExhaustsRetriesSurfacesLastError verifies a
// persistently failing endpoint yields the final attempt's error and no
// more than MaxAttempts calls.
func TestGenericHTTPPublisherExhaustsRetriesSurfacesLastError(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:     BackendGenericHTTP,
		WebhookURL:  srv.URL,
		Token:       "tok-prod-1a2b3c4d5e6f",
		HTTPClient:  srv.Client(),
		MaxAttempts: 3,
		BackoffBase: 2 * time.Millisecond,
		BackoffMax:  10 * time.Millisecond,
	})
	require.NoError(t, err)

	_, err = httpPub.Publish(context.Background(), approvedPublication(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "502")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 3, calls)
}

// TestGenericHTTPPublisherIdempotencyKey verifies a stable, deterministic key
// is attached across repeated attempts so the endpoint can deduplicate them.
func TestGenericHTTPPublisherIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	keys := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys[r.Header.Get("Idempotency-Key")]++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:             BackendGenericHTTP,
		WebhookURL:          srv.URL,
		Token:               "tok-prod-1a2b3c4d5e6f",
		HTTPClient:          srv.Client(),
		IdempotencyKeyField: "Idempotency-Key",
		MaxAttempts:         3,
		BackoffBase:         2 * time.Millisecond,
		BackoffMax:          10 * time.Millisecond,
	})
	require.NoError(t, err)

	pub := approvedPublication(t)
	_, err = httpPub.Publish(context.Background(), pub)
	require.Error(t, err) // all attempts 503

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, keys, 1, "all attempts must carry the identical idempotency key")
	for k, n := range keys {
		require.Equal(t, 3, n, "key %q seen every attempt", k)
		require.Equal(t, idempotencyKey(pub), k)
	}
}

// TestGenericHTTPPublisherHonorsExplicitIdempotencyKey verifies a caller-set
// IdempotencyKey on the publication is delivered verbatim.
func TestGenericHTTPPublisherHonorsExplicitIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("X-Idem")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:             BackendGenericHTTP,
		WebhookURL:          srv.URL,
		Token:               "tok-prod-1a2b3c4d5e6f",
		HTTPClient:          srv.Client(),
		IdempotencyKeyField: "X-Idem",
	})
	require.NoError(t, err)

	pub := approvedPublication(t)
	pub.IdempotencyKey = "caller-supplied-99"
	_, err = httpPub.Publish(context.Background(), pub)
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, "caller-supplied-99", got)
}

// TestGenericHTTPPublisherNoIdempotencyFieldWithoutConfig verifies no
// idempotency header is sent when the field is not configured.
func TestGenericHTTPPublisherNoIdempotencyFieldWithoutConfig(t *testing.T) {
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("Idempotency-Key")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	httpPub, err := NewGenericHTTPPublisher(PublisherConfig{
		Backend:    BackendGenericHTTP,
		WebhookURL: srv.URL,
		Token:      "tok-prod-1a2b3c4d5e6f",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, err = httpPub.Publish(context.Background(), approvedPublication(t))
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, got)
}
