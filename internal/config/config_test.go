package config

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// secureCore returns a Config whose required base settings are all secure so
// that tests can focus on the external-boundary validation rules in isolation.
func secureCore() *Config {
	return &Config{
		PostgresDSN:                "postgres://austro:austro@db:5432/austro?sslmode=disable",
		RedisAddr:                  "redis:6379",
		RabbitMQURL:                "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:                  "prod-access-secret-1234567890-abcdef",
		JWTRefreshSecret:           "prod-refresh-secret-1234567890-abcdef",
		AIBackend:                  AIBackendStub,
		PublishBackend:             PublishBackendStub,
		ExecutionTransportBackend:  ExecutionTransportBackendStub,
		ExecutionQueue:             DefaultExecutionQueue,
		ExecutionLeaseTTL:          60 * time.Second,
		ExecutionHeartbeatInterval: 20 * time.Second,
		ExecutionWindow:            50 * time.Second,
		ExecutionSweepInterval:     5 * time.Second,
		ExecutionRetryBackoffBase:  2 * time.Second,
		ExecutionRetryBackoffMax:   2 * time.Minute,
		HeartbeatInterval:          15 * time.Second,
		HeartbeatStaleAfter:        60 * time.Second,
	}
}

// TestValidateDefaultsToStubAdapters verifies the default (empty backend) and
// explicit stub adapters validate without any external configuration and that
// no external credential was accidentally required.
func TestValidateDefaultsToStubAdapters(t *testing.T) {
	cfg := secureCore()
	cfg.AIBackend = ""
	cfg.PublishBackend = ""
	require.NoError(t, cfg.Validate())
}

// TestValidateStubRejectsSuppliedAICredential verifies that supplying an AI
// credential while the stub adapter is selected fails fast, so a configured
// key is never silently ignored.
func TestValidateStubRejectsSuppliedAICredential(t *testing.T) {
	cfg := secureCore()
	cfg.AIAPIKey = "sk-prod-9f8e7d6c5b4a"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_API_KEY")
	require.NotContains(t, err.Error(), "sk-prod")
}

// TestValidateStubRejectsSuppliedPublishToken verifies the publishing analogue
// of the stub-rejects-credential rule.
func TestValidateStubRejectsSuppliedPublishToken(t *testing.T) {
	cfg := secureCore()
	cfg.PublishToken = "tok-prod-1a2b3c4d5e6f"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_TOKEN")
	require.NotContains(t, err.Error(), "tok-prod")
}

// TestValidateOpenAICompatibleRequiresCredentials verifies a real AI backend is
// CONFIGURATION_REQUIRED: omitting the key fails fast, names the offending
// setting, and never leaks the candidate secret.
func TestValidateOpenAICompatibleRequiresCredentials(t *testing.T) {
	cfg := secureCore()
	cfg.AIBackend = AIBackendOpenAICompatible
	cfg.AIModel = "gpt-test"
	cfg.AIBaseURL = "https://ai.austro.internal/v1"
	cfg.AIAPIKey = "sk-prod-9f8e7d6c5b4a"
	require.NoError(t, cfg.Validate())

	bad := secureCore()
	bad.AIBackend = AIBackendOpenAICompatible
	bad.AIModel = "gpt-test"
	bad.AIBaseURL = "https://ai.austro.internal/v1"
	bad.AIAPIKey = "sk-change-me-0000"
	err := bad.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_API_KEY")
	require.NotContains(t, err.Error(), "change-me")
}

// TestValidateOpenAICompatibleRejectsInsecureEndpoint verifies the endpoint is
// validated as a required, non-insecure setting.
func TestValidateOpenAICompatibleRejectsInsecureEndpoint(t *testing.T) {
	cfg := secureCore()
	cfg.AIBackend = AIBackendOpenAICompatible
	cfg.AIModel = "gpt-test"
	cfg.AIBaseURL = "https://ai.example.com/v1"
	cfg.AIAPIKey = "sk-prod-9f8e7d6c5b4a"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_BASE_URL")
}

// TestValidateLocalBackendAllowsNoAPIKey verifies the free-first "local"
// backend validates with Model and BaseURL while an API key stays optional: a
// local or in-house model endpoint often needs no credential, so the system
// must not require one to be runnable.
func TestValidateLocalBackendAllowsNoAPIKey(t *testing.T) {
	cfg := secureCore()
	cfg.AIBackend = AIBackendLocal
	cfg.AIModel = "local-model"
	cfg.AIBaseURL = "http://localhost:11434/v1"
	require.NoError(t, cfg.Validate())

	// A present, secure key is also accepted and preserves the credential.
	withKey := secureCore()
	withKey.AIBackend = AIBackendLocal
	withKey.AIModel = "local-model"
	withKey.AIBaseURL = "http://localhost:11434/v1"
	withKey.AIAPIKey = "local-token-1a2b3c4d5e6f"
	require.NoError(t, withKey.Validate())
}

// TestValidateLocalBackendRequiresModelAndBaseURL verifies the local backend
// still fails fast (CONFIGURATION_REQUIRED) when its endpoint identity is
// missing, naming both settings without leaking values.
func TestValidateLocalBackendRequiresModelAndBaseURL(t *testing.T) {
	cfg := secureCore()
	cfg.AIBackend = AIBackendLocal
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_MODEL")
	require.Contains(t, err.Error(), "AUSTRO_AI_BASE_URL")
}

// TestValidateLocalBackendRejectsInsecureAPIKey verifies a placeholder key is
// rejected even though the key itself is optional for the local backend.
func TestValidateLocalBackendRejectsInsecureAPIKey(t *testing.T) {
	cfg := secureCore()
	cfg.AIBackend = AIBackendLocal
	cfg.AIModel = "local-model"
	cfg.AIBaseURL = "http://localhost:11434/v1"
	cfg.AIAPIKey = "sk-change-me-0000"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_API_KEY")
	require.NotContains(t, err.Error(), "change-me")
}

// TestValidateGenericHTTPRequiresCredentials verifies a real publish backend is
// CONFIGURATION_REQUIRED and its token is validated without leaking the value.
func TestValidateGenericHTTPRequiresCredentials(t *testing.T) {
	cfg := secureCore()
	cfg.PublishBackend = PublishBackendGenericHTTP
	cfg.PublishWebhookURL = "https://hook.austro.internal/deliver"
	cfg.PublishToken = "tok-prod-1a2b3c4d5e6f"
	require.NoError(t, cfg.Validate())

	bad := secureCore()
	bad.PublishBackend = PublishBackendGenericHTTP
	bad.PublishWebhookURL = "https://hook.austro.internal/deliver"
	bad.PublishToken = "tok-example-0000"
	err := bad.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_TOKEN")
	require.NotContains(t, err.Error(), "tok-example")
}

// TestValidateRejectsUnknownBackend verifies an unrecognized adapter selector
// fails fast by setting name rather than degrading silently.
func TestValidateRejectsUnknownBackend(t *testing.T) {
	cfg := secureCore()
	cfg.AIBackend = "mystery-model"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_BACKEND")

	pub := secureCore()
	pub.PublishBackend = "mystery-http"
	err = pub.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_BACKEND")
}

// TestValidateRejectsMalformedUsageLimit verifies a malformed usage-limit value
// fails fast under strict validation instead of being silently treated as
// "unlimited" (fail-fast discipline for the external-boundary settings).
func TestValidateRejectsMalformedUsageLimit(t *testing.T) {
	cfg := secureCore()
	cfg.usageLimitRaw = "not-a-number"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE")
}

// TestLoadStrictRejectsMalformedUsageLimitEnv verifies the strict loader fails
// fast when the usage-limit environment value is not a valid unsigned integer.
func TestLoadStrictRejectsMalformedUsageLimitEnv(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE", "banana")

	_, err := LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE")
}

// TestLoadStrictRespectsBackendEnv verifies LoadStrict enforces the backend
// rules when values are supplied through the environment.
func TestLoadStrictRespectsBackendEnv(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")

	// Default stub backends with no external credentials must pass.
	_, err := LoadStrict()
	require.NoError(t, err)

	// A stub backend must reject a supplied credential via the environment.
	t.Setenv("AUSTRO_AI_API_KEY", "sk-prod-9f8e7d6c5b4a")
	_, err = LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_API_KEY")
}

// Set the full environment consumed by the composed runtime (the exact same
// variable names the API entrypoint and the worker entrypoint both read).
func fullRuntimeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_RABBITMQ_QUEUE", "austro.events.production")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_AI_BACKEND", AIBackendOpenAICompatible)
	t.Setenv("AUSTRO_AI_MODEL", "austro-classifier")
	t.Setenv("AUSTRO_AI_BASE_URL", "https://ai.austro.internal/v1")
	t.Setenv("AUSTRO_AI_API_KEY", "sk-prod-9f8e7d6c5b4a")
	t.Setenv("AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE", "10")
	t.Setenv("AUSTRO_PUBLISH_BACKEND", PublishBackendGenericHTTP)
	t.Setenv("AUSTRO_PUBLISH_WEBHOOK_URL", "https://hook.austro.internal/deliver")
	t.Setenv("AUSTRO_PUBLISH_TOKEN", "tok-prod-1a2b3c4d5e6f")
	t.Setenv("AUSTRO_PUBLISH_IDEMPOTENCY_FIELD", "Idempotency-Key")
	t.Setenv("AUSTRO_PUBLISH_MAX_ATTEMPTS", "5")
	t.Setenv("AUSTRO_PUBLISH_RETRY_BACKOFF_BASE", "500ms")
	t.Setenv("AUSTRO_PUBLISH_RETRY_BACKOFF_MAX", "30s")
	t.Setenv("AUSTRO_EXECUTION_TRANSPORT_BACKEND", ExecutionTransportBackendStub)
	t.Setenv("AUSTRO_EXECUTION_LEASE_TTL", "90s")
	t.Setenv("AUSTRO_EXECUTION_HEARTBEAT_INTERVAL", "30s")
	t.Setenv("AUSTRO_EXECUTION_WINDOW", "75s")
	t.Setenv("AUSTRO_EXECUTION_SWEEP_INTERVAL", "7s")
	t.Setenv("AUSTRO_EXECUTION_RETRY_BACKOFF_BASE", "3s")
	t.Setenv("AUSTRO_EXECUTION_RETRY_BACKOFF_MAX", "4m")
	t.Setenv("AUSTRO_EXECUTION_WORKSPACE_IDS", "16dd4e23-8b1a-4b82-9f3c-9f3c9f3c9f3c, 2cbea6f0-6f6d-4be0-9a8d-e9c8d0f0a1b2")
}

// TestLoadStrictSingleCanonicalEnvironment proves both runtime entrypoints
// (API and worker) share one configuration model: every value the composed
// runtime consumes is parsed from the same variable names, and none is lost.
func TestLoadStrictSingleCanonicalEnvironment(t *testing.T) {
	fullRuntimeEnv(t)

	cfg, err := LoadStrict()
	require.NoError(t, err)

	require.Equal(t, AIBackendOpenAICompatible, cfg.AIBackend)
	require.Equal(t, "austro-classifier", cfg.AIModel)
	require.Equal(t, "https://ai.austro.internal/v1", cfg.AIBaseURL)
	require.Equal(t, "sk-prod-9f8e7d6c5b4a", cfg.AIAPIKey)
	require.Equal(t, uint64(10), cfg.AIUsageLimitPerWorkspace)
	require.Equal(t, PublishBackendGenericHTTP, cfg.PublishBackend)
	require.Equal(t, "https://hook.austro.internal/deliver", cfg.PublishWebhookURL)
	require.Equal(t, "tok-prod-1a2b3c4d5e6f", cfg.PublishToken)
	require.Equal(t, "Idempotency-Key", cfg.PublishIdempotencyField)
	require.Equal(t, 5, cfg.PublishMaxAttempts)
	require.Equal(t, 500*time.Millisecond, cfg.PublishRetryBackoffBase)
	require.Equal(t, 30*time.Second, cfg.PublishRetryBackoffMax)
	require.Equal(t, "austro.events.production", cfg.RabbitMQQueue)
	require.Equal(t, ExecutionTransportBackendStub, cfg.ExecutionTransportBackend)
	require.Equal(t, 90*time.Second, cfg.ExecutionLeaseTTL)
	require.Equal(t, 30*time.Second, cfg.ExecutionHeartbeatInterval)
	require.Equal(t, 75*time.Second, cfg.ExecutionWindow)
	require.Equal(t, 7*time.Second, cfg.ExecutionSweepInterval)
	require.Equal(t, 3*time.Second, cfg.ExecutionRetryBackoffBase)
	require.Equal(t, 4*time.Minute, cfg.ExecutionRetryBackoffMax)
	require.Len(t, cfg.ExecutionWorkspaceIDs, 2)
	require.Equal(t, "16dd4e23-8b1a-4b82-9f3c-9f3c9f3c9f3c", cfg.ExecutionWorkspaceIDs[0].String())
}

// TestValidateRejectsNegativeRetrySettings verifies the panic-prone negative
// attempt/backoff bounds are rejected by strict validation (fail-fast).
func TestValidateRejectsNegativeRetrySettings(t *testing.T) {
	cfg := secureCore()
	cfg.PublishMaxAttempts = -1
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_MAX_ATTEMPTS")

	bad := secureCore()
	bad.PublishRetryBackoffBase = -time.Second
	err = bad.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_RETRY_BACKOFF")
}

// TestLoadStrictFailsClosedOnMissingRealBackendConfig proves the worker and API
// fail closed when a real backend is selected without its required secrets.
func TestLoadStrictFailsClosedOnMissingRealBackendConfig(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")

	// AI backend real but API key absent.
	t.Setenv("AUSTRO_AI_BACKEND", AIBackendOpenAICompatible)
	t.Setenv("AUSTRO_AI_MODEL", "austro-classifier")
	t.Setenv("AUSTRO_AI_BASE_URL", "https://ai.austro.internal/v1")
	_, err := LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_API_KEY")

	// Publishing backend real but token absent.
	t.Setenv("AUSTRO_AI_BACKEND", AIBackendStub)
	t.Setenv("AUSTRO_PUBLISH_BACKEND", PublishBackendGenericHTTP)
	t.Setenv("AUSTRO_PUBLISH_WEBHOOK_URL", "https://hook.austro.internal/deliver")
	_, err = LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_TOKEN")
}

// TestLoadStubModeSafeAndDeterministic proves the default stub mode needs no
// external configuration, honours a configured usage limit, and never carries a
// credential that could trigger an external call.
func TestLoadStubModeSafeAndDeterministic(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE", "10")

	cfg, err := LoadStrict()
	require.NoError(t, err)
	require.Equal(t, AIBackendStub, cfg.AIBackend)
	require.Equal(t, PublishBackendStub, cfg.PublishBackend)
	require.Equal(t, uint64(10), cfg.AIUsageLimitPerWorkspace)
	require.Empty(t, cfg.AIAPIKey)
	require.Empty(t, cfg.AIBaseURL)
	require.Empty(t, cfg.PublishToken)

	// Re-load gives the exact same configuration (deterministic model).
	cfg2, err := LoadStrict()
	require.NoError(t, err)
	require.Equal(t, cfg, cfg2)
}

// TestValidateRejectsShortJWTSecrets verifies the HS256 key floor. A secret
// that is not a known placeholder but is shorter than the hash output is still
// below the algorithm's design strength (RFC 7518 §3.2), so it must fail fast
// rather than be accepted because it merely looks random.
func TestValidateRejectsShortJWTSecrets(t *testing.T) {
	short := secureCore()
	short.JWTSecret = "s3cr3t"
	err := short.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_JWT_SECRET")
	require.NotContains(t, err.Error(), "s3cr3t", "the secret value must never be echoed")

	shortRefresh := secureCore()
	shortRefresh.JWTRefreshSecret = "s3cr3t-refresh"
	err = shortRefresh.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_JWT_REFRESH_SECRET")

	// Exactly at the floor is accepted; one byte under is not.
	atFloor := secureCore()
	atFloor.JWTSecret = strings.Repeat("k", minHMACKeyBytes)
	atFloor.JWTRefreshSecret = strings.Repeat("k", minHMACKeyBytes)
	require.NoError(t, atFloor.Validate())

	underFloor := secureCore()
	underFloor.JWTSecret = strings.Repeat("k", minHMACKeyBytes-1)
	err = underFloor.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_JWT_SECRET")
}

// TestValidateRejectsShortFounderPassword verifies the founder credential has a
// password floor, not just a placeholder check.
func TestValidateRejectsShortFounderPassword(t *testing.T) {
	cfg := secureCore()
	cfg.FounderUsername = "founder"
	cfg.FounderPassword = "hunter2"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_FOUNDER_PASSWORD")

	ok := secureCore()
	ok.FounderUsername = "founder"
	ok.FounderPassword = strings.Repeat("p", minFounderPasswordBytes)
	require.NoError(t, ok.Validate())
}

// TestLoadStrictRejectsUnparseableNumericSettings verifies a supplied-but
// malformed budget setting fails fast instead of being silently replaced by the
// default. A typo in a retry budget must not look like an intentional unset.
func TestLoadStrictRejectsUnparseableNumericSettings(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")

	t.Setenv("AUSTRO_PUBLISH_MAX_ATTEMPTS", "many")
	_, err := LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_MAX_ATTEMPTS")

	t.Setenv("AUSTRO_PUBLISH_MAX_ATTEMPTS", "5")
	t.Setenv("AUSTRO_PUBLISH_RETRY_BACKOFF_BASE", "soon")
	_, err = LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_RETRY_BACKOFF_BASE")

	t.Setenv("AUSTRO_PUBLISH_RETRY_BACKOFF_BASE", "500ms")
	t.Setenv("AUSTRO_PUBLISH_RETRY_BACKOFF_MAX", "30")
	_, err = LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_RETRY_BACKOFF_MAX")

	// Well-formed values still load.
	t.Setenv("AUSTRO_PUBLISH_RETRY_BACKOFF_MAX", "30s")
	cfg, err := LoadStrict()
	require.NoError(t, err)
	require.Equal(t, 5, cfg.PublishMaxAttempts)
	require.Equal(t, 500*time.Millisecond, cfg.PublishRetryBackoffBase)
	require.Equal(t, 30*time.Second, cfg.PublishRetryBackoffMax)
}

// TestValidateErrorMessageIsDeterministicAndDeduplicated verifies the same
// misconfiguration always renders the same message and names each offending
// setting once, even when several rules flag it.
func TestValidateErrorMessageIsDeterministicAndDeduplicated(t *testing.T) {
	cfg := secureCore()
	cfg.JWTSecret = "change-me" // flagged by both the placeholder and the length rule
	first := cfg.Validate()
	require.Error(t, first)
	for i := 0; i < 25; i++ {
		require.Equal(t, first.Error(), cfg.Validate().Error(),
			"the validation error must not depend on map iteration order")
	}
	require.Equal(t, 1, strings.Count(first.Error(), "AUSTRO_JWT_SECRET"),
		"a setting flagged by two rules must be named once")
}

// TestValidateRejectsBadExecutionRuntimeSettings verifies the execution runtime
// durations must be positive and satisfy the pacing relationships the
// dispatcher/worker rely on; zero/negative values and a heartbeat, window or
// backoff cap that violate the lease are configuration errors, not runtime
// panics.
func TestValidateRejectsBadExecutionRuntimeSettings(t *testing.T) {
	zero := secureCore()
	zero.ExecutionLeaseTTL = 0
	err := zero.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_LEASE_TTL")

	negative := secureCore()
	negative.ExecutionRetryBackoffBase = -time.Second
	err = negative.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_RETRY_BACKOFF_BASE")

	badHeartbeat := secureCore()
	badHeartbeat.ExecutionHeartbeatInterval = badHeartbeat.ExecutionLeaseTTL
	err = badHeartbeat.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_HEARTBEAT_INTERVAL")

	badWindow := secureCore()
	badWindow.ExecutionWindow = badWindow.ExecutionLeaseTTL + time.Second
	err = badWindow.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_WINDOW")

	badBackoff := secureCore()
	badBackoff.ExecutionRetryBackoffBase = 5 * time.Minute
	err = badBackoff.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_RETRY_BACKOFF")
}

// TestValidateRejectsUnknownExecutionTransport verifies an unrecognized
// execution dispatch transport selector fails fast by setting name rather
// than silently falling back to the stub.
func TestValidateRejectsUnknownExecutionTransport(t *testing.T) {
	cfg := secureCore()
	cfg.ExecutionTransportBackend = "carrier-pigeon"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_TRANSPORT_BACKEND")
	require.NotContains(t, err.Error(), "carrier-pigeon")
}

// TestValidateAcceptsRabbitMQExecutionTransport verifies the broker-backed
// transport is a valid selector under validation (it requires no resting
// credential; the entrypoint dials the configured RabbitMQ URL).
func TestValidateAcceptsRabbitMQExecutionTransport(t *testing.T) {
	cfg := secureCore()
	cfg.ExecutionTransportBackend = ExecutionTransportBackendRabbitMQ
	require.NoError(t, cfg.Validate())
}

// TestValidateRejectsMalformedWorkspaceIDs verifies AUSTRO_EXECUTION_WORKSPACE_IDS
// must be a comma-separated list of UUIDs: a malformed token fails fast instead
// of silently dispatching for a partial list.
func TestValidateRejectsMalformedWorkspaceIDs(t *testing.T) {
	cfg := secureCore()
	cfg.executionWorkspaceIDsRaw = "16dd4e23-8b1a-4b82-9f3c-9f3c9f3c9f3c, not-a-uuid"
	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_WORKSPACE_IDS")

	ok := secureCore()
	ok.executionWorkspaceIDsRaw = "16dd4e23-8b1a-4b82-9f3c-9f3c9f3c9f3c"
	ok.ExecutionWorkspaceIDs = []uuid.UUID{mustParse(t, ok.executionWorkspaceIDsRaw)}
	require.NoError(t, ok.Validate())
}

func mustParse(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	require.NoError(t, err)
	return id
}

// productionCore returns a secureCore whose external boundaries are all real,
// which is the only shape a production deployment is allowed to start in.
func productionCore(t *testing.T) *Config {
	t.Helper()
	cfg := secureCore()
	cfg.Environment = EnvironmentProduction
	cfg.AIBackend = AIBackendOpenAICompatible
	cfg.AIModel = "gpt-4o-mini"
	cfg.AIBaseURL = "https://ai.austro.internal/v1"
	cfg.AIAPIKey = "sk-prod-9f8e7d6c5b4a3d2e"
	cfg.PublishBackend = PublishBackendGenericHTTP
	cfg.PublishWebhookURL = "https://hook.austro.internal/deliver"
	cfg.PublishToken = "tok-prod-1a2b3c4d5e6f"
	cfg.ExecutionTransportBackend = ExecutionTransportBackendRabbitMQ
	cfg.executionWorkspaceIDsRaw = "16dd4e23-8b1a-4b82-9f3c-9f3c9f3c9f3c"
	cfg.ExecutionWorkspaceIDs = []uuid.UUID{mustParse(t, cfg.executionWorkspaceIDsRaw)}
	return cfg
}

// offlineProductionCore returns a production configuration that runs the
// deterministic stubs with no external settings at all.
//
// The external settings are cleared as well as the backend selectors, because
// the pre-existing rule refuses a credential supplied while a stub is selected
// (a configured key must never sit ignored in the environment). That is the
// procedure an operator actually follows when taking a deployment offline —
// remove the credentials, do not just flip the backend — so the fixture
// reproduces it, and every production rule below then fails or passes for
// exactly one reason.
func offlineProductionCore(t *testing.T) *Config {
	t.Helper()
	cfg := productionCore(t)
	cfg.AIBackend = AIBackendStub
	cfg.AIModel = ""
	cfg.AIBaseURL = ""
	cfg.AIAPIKey = ""
	cfg.PublishBackend = PublishBackendStub
	cfg.PublishWebhookURL = ""
	cfg.PublishToken = ""
	cfg.ExecutionTransportBackend = ExecutionTransportBackendStub
	cfg.ExecutionWorkspaceIDs = nil
	return cfg
}

// TestValidateRejectsBlankExecutionQueue verifies the dispatch queue name
// cannot be blank. The dispatcher and the worker are separate processes and the
// name is the only thing that makes them meet, so an empty value would be a
// configuration in which both halves start and no work ever crosses between
// them.
func TestValidateRejectsBlankExecutionQueue(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t"} {
		cfg := secureCore()
		cfg.ExecutionQueue = blank
		err := cfg.Validate()
		require.ErrorIs(t, err, ErrConfigInvalid, "queue name %q must be rejected", blank)
		require.Contains(t, err.Error(), "AUSTRO_EXECUTION_QUEUE")
	}
}

// TestProductionRejectsStubAdapters is the release gate: a deployment that
// declares itself production may not run any deterministic stub. Every stub
// answers successfully without contacting a real service, so this is the
// configuration that serves traffic, reports healthy, and does no real work.
func TestProductionRejectsStubAdapters(t *testing.T) {
	cfg := offlineProductionCore(t)

	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	for _, name := range []string{"AUSTRO_AI_BACKEND", "AUSTRO_PUBLISH_BACKEND", "AUSTRO_EXECUTION_TRANSPORT_BACKEND"} {
		require.Contains(t, err.Error(), name)
	}
}

// TestProductionRejectsEachStubIndividually verifies the three boundaries are
// each refused on their own, so fixing one never silently leaves the other two
// running as stubs.
func TestProductionRejectsEachStubIndividually(t *testing.T) {
	t.Run("ai", func(t *testing.T) {
		cfg := offlineProductionCore(t)
		cfg.AIBackend = AIBackendStub
		require.Contains(t, cfg.Validate().Error(), "AUSTRO_AI_BACKEND")
	})
	t.Run("publish", func(t *testing.T) {
		cfg := offlineProductionCore(t)
		cfg.PublishBackend = PublishBackendStub
		require.Contains(t, cfg.Validate().Error(), "AUSTRO_PUBLISH_BACKEND")
	})
	t.Run("execution transport", func(t *testing.T) {
		cfg := productionCore(t)
		cfg.ExecutionTransportBackend = ExecutionTransportBackendStub
		require.Contains(t, cfg.Validate().Error(), "AUSTRO_EXECUTION_TRANSPORT_BACKEND")
	})
	t.Run("implicit empty ai backend", func(t *testing.T) {
		cfg := offlineProductionCore(t)
		cfg.AIBackend = ""
		require.Contains(t, cfg.Validate().Error(), "AUSTRO_AI_BACKEND")
	})
}

// TestProductionStubRuleIgnoresEnvironmentCasing verifies the production rule
// cannot be stepped around by writing "Production" or leaving whitespace in the
// label: an environment that only half activates its safety rules is worse than
// one that never activates them, because it reads as configured.
func TestProductionStubRuleIgnoresEnvironmentCasing(t *testing.T) {
	for _, env := range []string{"Production", "PRODUCTION", " production "} {
		cfg := productionCore(t)
		cfg.Environment = env
		cfg.AIBackend = AIBackendStub
		err := cfg.Validate()
		require.ErrorIs(t, err, ErrConfigInvalid, "environment %q must activate the production rules", env)
		require.Contains(t, err.Error(), "AUSTRO_AI_BACKEND")
	}
}

// TestProductionRejectsRabbitMQTransportWithoutWorkspaces verifies a
// broker-backed dispatcher with no workspaces to sweep is a startup failure in
// production. It would otherwise connect, log that it started, and never
// dispatch anything: a silently broken subsystem wearing the costume of a
// working one.
func TestProductionRejectsRabbitMQTransportWithoutWorkspaces(t *testing.T) {
	cfg := productionCore(t)
	cfg.executionWorkspaceIDsRaw = ""
	cfg.ExecutionWorkspaceIDs = nil

	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_WORKSPACE_IDS")
}

// TestAllowStubBackendsPermitsAnOfflineProductionStack verifies the documented
// opt-in really does permit the stubs in production. A refusal an operator
// cannot legally resolve is a bug, not a safety property: the deployment that
// genuinely has no egress needs a visible, greppable way to say so.
func TestAllowStubBackendsPermitsAnOfflineProductionStack(t *testing.T) {
	cfg := offlineProductionCore(t)
	cfg.AllowStubBackends = true

	require.NoError(t, cfg.Validate())
}

// TestAllowStubBackendsRejectsUnparseableValue verifies the opt-in fails
// closed. A typo that read as consent would grant exactly the exemption the
// operator did not intend.
func TestAllowStubBackendsRejectsUnparseableValue(t *testing.T) {
	cfg := offlineProductionCore(t)
	cfg.allowStubBackendsRaw = "yes-please"

	err := cfg.Validate()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_ALLOW_STUB_BACKENDS")
}

// TestProductionAcceptsFullyConfiguredStack is the positive counterpart: a real
// deployment with every boundary configured, at least one workspace to sweep and
// a named queue validates cleanly. Without this the production rules would
// simply be a wall.
func TestProductionAcceptsFullyConfiguredStack(t *testing.T) {
	require.NoError(t, productionCore(t).Validate())
}

// TestProductionAcceptsLocalKeylessAIBackend verifies the keyless local backend
// is a legitimate production choice, so the gate does not push an operator
// toward buying an API key they do not need.
func TestProductionAcceptsLocalKeylessAIBackend(t *testing.T) {
	cfg := productionCore(t)
	cfg.AIBackend = AIBackendLocal
	cfg.AIModel = "llama-3.1-8b"
	cfg.AIBaseURL = "http://localhost:11434/v1"
	cfg.AIAPIKey = ""

	require.NoError(t, cfg.Validate())
}

// TestProductionWorkspaceRuleIsScopedToTheBrokerTransport verifies the
// workspace requirement applies to a real transport only. Requiring workspaces
// for a stub transport would be noise: a stub dispatch loop has nothing to
// deliver anywhere.
func TestProductionWorkspaceRuleIsScopedToTheBrokerTransport(t *testing.T) {
	cfg := offlineProductionCore(t)
	cfg.AllowStubBackends = true

	require.NoError(t, cfg.Validate())
}

// TestOfflineDevelopmentIsUnaffectedByTheProductionRule verifies the release
// gate does not leak into development: the stub defaults must keep working with
// no configuration at all, which is the property that makes the project
// runnable by anyone who clones it.
func TestOfflineDevelopmentIsUnaffectedByTheProductionRule(t *testing.T) {
	for _, env := range []string{"", "development", "test", "staging", "Production-like"} {
		cfg := secureCore()
		cfg.Environment = env
		require.NoError(t, cfg.Validate(), "environment %q must stay on the offline defaults", env)
	}
}

// TestLoadStrictAppliesTheProductionRuleThroughTheEnvironment verifies the gate
// is reached by the real entrypoint path, not only by a hand-built struct: a
// rule that only tests could trip would not protect a deployment.
func TestLoadStrictAppliesTheProductionRuleThroughTheEnvironment(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_ENV", "production")
	t.Setenv("AUSTRO_AI_BACKEND", "stub")
	t.Setenv("AUSTRO_PUBLISH_BACKEND", "stub")
	t.Setenv("AUSTRO_EXECUTION_TRANSPORT_BACKEND", "rabbitmq")
	t.Setenv("AUSTRO_EXECUTION_WORKSPACE_IDS", "16dd4e23-8b1a-4b82-9f3c-9f3c9f3c9f3c")

	_, err := LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_AI_BACKEND")
	require.Contains(t, err.Error(), "AUSTRO_PUBLISH_BACKEND")
}

// TestLoadStrictDefaultsTheExecutionQueue verifies the queue name is
// deployment configuration with a documented default, read through the real
// loader: unset yields the name the broker and the worker already use, and a
// supplied value is honoured verbatim so a deployment may rename the queue in
// one place.
func TestLoadStrictDefaultsTheExecutionQueue(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")

	t.Setenv("AUSTRO_EXECUTION_QUEUE", "")
	cfg, err := LoadStrict()
	require.NoError(t, err)
	require.Equal(t, DefaultExecutionQueue, cfg.ExecutionQueue)

	t.Setenv("AUSTRO_EXECUTION_QUEUE", "austro.executions.prod")
	cfg, err = LoadStrict()
	require.NoError(t, err)
	require.Equal(t, "austro.executions.prod", cfg.ExecutionQueue)
}

// TestLoadStrictRejectsUnparseableExecutionSettings verifies a supplied-but
// malformed execution duration fails fast under strict validation instead of
// being silently replaced by the default.
func TestLoadStrictRejectsUnparseableExecutionSettings(t *testing.T) {
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")

	t.Setenv("AUSTRO_EXECUTION_LEASE_TTL", "almost")
	_, err := LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_LEASE_TTL")

	t.Setenv("AUSTRO_EXECUTION_LEASE_TTL", "60s")
	t.Setenv("AUSTRO_EXECUTION_RETRY_BACKOFF_MAX", "30")
	_, err = LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_RETRY_BACKOFF_MAX")

	// Well-formed values still load, and the third phase must be immune to
	// whatever pacing-related execution timings the surrounding environment
	// already exports: the 60s lease chosen here only stays consistent if the
	// window and heartbeat are re-stated well under it.
	t.Setenv("AUSTRO_EXECUTION_RETRY_BACKOFF_MAX", "2m")
	t.Setenv("AUSTRO_EXECUTION_WINDOW", "40s")
	t.Setenv("AUSTRO_EXECUTION_HEARTBEAT_INTERVAL", "20s")
	cfg, err := LoadStrict()
	require.NoError(t, err)
	require.Equal(t, 60*time.Second, cfg.ExecutionLeaseTTL)
	require.Equal(t, 2*time.Minute, cfg.ExecutionRetryBackoffMax)
}
