package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// secureCore returns a Config whose required base settings are all secure so
// that tests can focus on the external-boundary validation rules in isolation.
func secureCore() *Config {
	return &Config{
		PostgresDSN:      "postgres://austro:austro@db:5432/austro?sslmode=disable",
		RedisAddr:        "redis:6379",
		RabbitMQURL:      "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:        "prod-access-secret-1234567890-abcdef",
		JWTRefreshSecret: "prod-refresh-secret-1234567890-abcdef",
		AIBackend:        AIBackendStub,
		PublishBackend:   PublishBackendStub,
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
