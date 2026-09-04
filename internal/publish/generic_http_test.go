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
