package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// mustScope returns a valid gateway scope for the given workspace.
func mustScope(ws string) Scope {
	return Scope{WorkspaceID: ws, TraceID: uuid.New(), SpanID: uuid.New()}
}

// TestOpenAICompatibleComplete verifies Complete issues an authenticated
// request, sends the configured model, and returns the choice text.
func TestOpenAICompatibleComplete(t *testing.T) {
	var gotKey, gotModel string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotKey = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello-result"}}]}`))
	}))
	defer srv.Close()

	p, err := NewOpenAICompatibleProvider(ProviderConfig{
		Backend:    BackendOpenAICompatible,
		Model:      "gpt-test",
		BaseURL:    srv.URL,
		APIKey:     "sk-prod-9f8e7d6c5b4a",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	res, err := p.Complete(context.Background(), CompletionRequest{
		Scope:       mustScope("ws-a"),
		Instruction: "write a script",
	})
	require.NoError(t, err)
	require.Equal(t, "hello-result", res.Text)

	mu.Lock()
	require.Equal(t, "Bearer sk-prod-9f8e7d6c5b4a", gotKey)
	require.Equal(t, "gpt-test", gotModel)
	mu.Unlock()
}

// TestOpenAICompatibleCompleteRequiresScope verifies an absent workspace is
// rejected before any outbound call.
func TestOpenAICompatibleCompleteRequiresScope(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	p, err := NewOpenAICompatibleProvider(ProviderConfig{
		Backend:    BackendOpenAICompatible,
		Model:      "gpt-test",
		BaseURL:    srv.URL,
		APIKey:     "sk-prod-9f8e7d6c5b4a",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	_, err = p.Complete(context.Background(), CompletionRequest{Instruction: "x"})
	require.ErrorIs(t, err, ErrWorkspaceRequired)
	require.False(t, called, "no outbound call must be made without a workspace")
}

// TestOpenAICompatibleEmbed verifies Embed posts and returns a vector sized to
// Dimensions, truncating a longer provider vector.
func TestOpenAICompatibleEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3,0.4]}]}`))
	}))
	defer srv.Close()
	p, err := NewOpenAICompatibleProvider(ProviderConfig{
		Backend:    BackendOpenAICompatible,
		Model:      "embed-test",
		BaseURL:    srv.URL,
		APIKey:     "sk-prod-9f8e7d6c5b4a",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	res, err := p.Embed(context.Background(), EmbedRequest{
		Scope:      mustScope("ws-a"),
		Content:    "embed me",
		Dimensions: 2,
	})
	require.NoError(t, err)
	require.Equal(t, 2, res.Dimensions)
	require.Equal(t, []float32{0.1, 0.2}, res.Values)
}

// TestOpenAICompatibleClassify verifies Classify maps a returned label to one
// of the requested categories and falls back to the first on unrecognized text.
func TestOpenAICompatibleClassify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"scratch"}}]}`))
	}))
	defer srv.Close()
	p, err := NewOpenAICompatibleProvider(ProviderConfig{
		Backend:    BackendOpenAICompatible,
		Model:      "class-test",
		BaseURL:    srv.URL,
		APIKey:     "sk-prod-9f8e7d6c5b4a",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	res, err := p.Classify(context.Background(), ClassificationRequest{
		Scope:      mustScope("ws-a"),
		Categories: []string{"script", "thumbnail", "scratch"},
		Input:      "some input",
	})
	require.NoError(t, err)
	require.Equal(t, "scratch", res.Category)
}

// TestOpenAICompatibleErrorNeverLeaksKey verifies a provider error status is
// surfaced without echoing the API key.
func TestOpenAICompatibleErrorNeverLeaksKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p, err := NewOpenAICompatibleProvider(ProviderConfig{
		Backend:    BackendOpenAICompatible,
		Model:      "gpt-test",
		BaseURL:    srv.URL,
		APIKey:     "sk-prod-9f8e7d6c5b4a",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	_, err = p.Complete(context.Background(), CompletionRequest{
		Scope:       mustScope("ws-a"),
		Instruction: "x",
	})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "sk-prod")
	require.Contains(t, err.Error(), "401")
}

// TestOpenAICompatibleEmptyChoices verifies an empty completion is an error.
func TestOpenAICompatibleEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	p, err := NewOpenAICompatibleProvider(ProviderConfig{
		Backend:    BackendOpenAICompatible,
		Model:      "gpt-test",
		BaseURL:    srv.URL,
		APIKey:     "sk-prod-9f8e7d6c5b4a",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	_, err = p.Complete(context.Background(), CompletionRequest{
		Scope:       mustScope("ws-a"),
		Instruction: "x",
	})
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "no completion")
}

// TestOpenAICompatibleOversizedResponseRejected verifies an oversized provider
// response fails the operation instead of buffering unbounded memory.
func TestOpenAICompatibleOversizedResponseRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxResponseBytes+1))
	}))
	defer srv.Close()
	p, err := NewOpenAICompatibleProvider(ProviderConfig{
		Backend:    BackendOpenAICompatible,
		Model:      "gpt-test",
		BaseURL:    srv.URL,
		APIKey:     "sk-prod-9f8e7d6c5b4a",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)
	_, err = p.Complete(context.Background(), CompletionRequest{
		Scope:       mustScope("ws-a"),
		Instruction: "x",
	})
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "too large")
}
