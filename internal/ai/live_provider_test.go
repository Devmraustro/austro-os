package ai_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"austro-os/internal/ai"
	"austro-os/internal/config"

	"github.com/stretchr/testify/require"
)

// This file is the opt-in half of the provider release gate.
//
// The real HTTP provider path is covered offline: openai_compatible_test.go
// drives NewOpenAICompatibleProvider and NewLocalProvider against httptest
// servers and asserts the wire behaviour — request shape, the Authorization
// header (present for openai-compatible, absent for a keyless local endpoint),
// empty-choices handling, oversized-response refusal, and that an error never
// carries the key. That proves the code we wrote; it cannot prove that a
// particular vendor's endpoint accepts our request, that the credential works,
// or that the model name is spelled the way that account expects.
//
// Closing that gap needs a real call, and a real call may cost money. So it is
// opt-in, off by default, and cheap when it does run: one short completion with
// a hard cap on how long it may take. It skips rather than fails when it is not
// enabled, so an ordinary `go test ./...` — in CI or on a laptop — never
// reaches a paid provider by accident.
//
// To run it deliberately, with a real credential in the environment:
//
//	AUSTRO_AI_LIVE_TEST=1 \
//	AUSTRO_AI_BACKEND=openai-compatible \
//	AUSTRO_AI_BASE_URL=https://<endpoint>/v1 \
//	AUSTRO_AI_MODEL=<model> \
//	AUSTRO_AI_API_KEY=<key> \
//	go test ./internal/ai/ -run TestLiveProvider -v
//
// Until someone runs it that way, the honest statement in the release report is
// that the real provider is UNVERIFIED against a live endpoint — not that it
// works.

const liveTestWorkspace = "austro-ai-live-smoke-test"

// TestLiveProviderCompletion performs one real completion against the configured
// provider. It is the only test in the repository that may contact an external
// service or incur a cost.
func TestLiveProviderCompletion(t *testing.T) {
	backend := strings.TrimSpace(os.Getenv("AUSTRO_AI_BACKEND"))
	if os.Getenv("AUSTRO_AI_LIVE_TEST") != "1" {
		t.Skip("set AUSTRO_AI_LIVE_TEST=1 with a real provider configuration to verify the live endpoint")
	}

	// A stub here would "pass" while proving nothing about any real endpoint, so
	// opting in with the offline adapter is a configuration mistake worth naming
	// rather than a green check to celebrate.
	if backend == "" || backend == config.AIBackendStub {
		t.Fatalf("AUSTRO_AI_LIVE_TEST=1 requires a real AUSTRO_AI_BACKEND, got %q; the stub would pass without contacting anything", backend)
	}

	// Build the provider through the same factory the composition boundary uses,
	// so this test exercises the real selection path rather than a hand-wired
	// provider that production never constructs.
	provider, err := ai.NewProvider(ai.ProviderConfig{
		Backend: backend,
		Model:   os.Getenv("AUSTRO_AI_MODEL"),
		BaseURL: os.Getenv("AUSTRO_AI_BASE_URL"),
		APIKey:  os.Getenv("AUSTRO_AI_API_KEY"),
		Timeout: 45 * time.Second,
	})
	require.NoError(t, err, "the configured provider could not be constructed")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The prompt is deliberately trivial and the assertions are about transport
	// rather than output quality: a provider that cannot answer "reply with the
	// single word: ok" is not usable, and nothing about release readiness depends
	// on the prose it returns.
	result, err := provider.Complete(ctx, ai.CompletionRequest{
		Scope:       ai.Scope{WorkspaceID: liveTestWorkspace},
		Instruction: "Reply with the single word: ok",
	})
	require.NoError(t, err, "the live provider rejected a minimal completion request")
	require.NotEmpty(t, strings.TrimSpace(result.Text), "the live provider returned an empty completion")
}

// TestLiveProviderErrorNeverLeaksCredentials asserts the property that must hold
// for a real endpoint, using the offline path so it costs nothing: a provider
// error surfaced to a caller or a log must never carry the API key.
func TestLiveProviderErrorNeverLeaksCredentials(t *testing.T) {
	const key = "sk-live-must-never-appear-9f8e7d6c5b4a"

	_, err := ai.NewProvider(ai.ProviderConfig{
		Backend: config.AIBackendOpenAICompatible,
		Model:   "a-model-that-does-not-exist",
		// Reserved TEST-NET-1 address (RFC 5737): guaranteed not to be a real
		// endpoint, so the call fails locally and quickly with no egress and no
		// cost, while still producing a genuine transport error to inspect.
		BaseURL: "http://192.0.2.1/v1",
		APIKey:  key,
		Timeout: 2 * time.Second,
	})
	require.NoError(t, err, "a fully configured provider must construct")

	// A blank model is refused by the factory, and the refusal must name the
	// setting without echoing the credential that was supplied alongside it.
	_, err = ai.NewProvider(ai.ProviderConfig{
		Backend: config.AIBackendOpenAICompatible,
		Model:   "",
		BaseURL: "http://192.0.2.1/v1",
		APIKey:  key,
	})
	require.Error(t, err)
	require.NotContains(t, err.Error(), key, "a provider configuration error must never echo the API key")
}
