package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	logger "austro-os/internal/log"
)

// Reserved adapter names. The default (stub) adapters are the only ones that
// may run without external configuration: they never contact a real external
// service and hold no credential. Selecting any other backend requires the
// corresponding external settings to be supplied securely (see Validate).
const (
	// AIBackendStub is the deterministic, offline AI adapter (default).
	AIBackendStub = "stub"
	// AIBackendOpenAICompatible is a real OpenAI-compatible HTTP endpoint.
	AIBackendOpenAICompatible = "openai-compatible"
	// PublishBackendStub is the deterministic, offline publishing adapter (default).
	PublishBackendStub = "stub"
	// PublishBackendGenericHTTP is a real generic HTTP delivery endpoint.
	PublishBackendGenericHTTP = "generic-http"
)

type Config struct {
	ServerAddress    string
	PostgresDSN      string
	RedisAddr        string
	RabbitMQURL      string
	RabbitMQQueue    string
	JWTSecret        string
	JWTRefreshSecret string
	Environment      string
	WorkspaceID      string

	// AIBackend selects the AI Provider adapter (AIBackendStub by default).
	// A non-stub backend is CONFIGURATION_REQUIRED: AIModel, AIBaseURL and
	// AIAPIKey must be present and secure, or validation fails fast.
	AIBackend string
	// AIModel is the model identifier used by a non-stub AI backend.
	AIModel string
	// AIBaseURL is the OpenAI-compatible endpoint base URL (non-stub only).
	AIBaseURL string
	// AIAPIKey is the credential for the AI backend (non-stub only). It is
	// never logged and never echoed into validation errors.
	AIAPIKey string
	// AIUsageLimitPerWorkspace caps total AI operations per workspace (0 =
	// no ceiling; forwarded to the usage guard).
	AIUsageLimitPerWorkspace uint64
	// usageLimitRaw is the raw environment value for AIUsageLimitPerWorkspace.
	// Strict validation rejects a value that does not parse as an unsigned
	// integer (fail-fast) rather than silently treating it as "no ceiling".
	usageLimitRaw string

	// PublishBackend selects the Publisher adapter (PublishBackendStub by
	// default). A non-stub backend is CONFIGURATION_REQUIRED: PublishWebhookURL
	// and PublishToken must be present and secure.
	PublishBackend string
	// PublishWebhookURL is the external delivery endpoint (non-stub only).
	PublishWebhookURL string
	// PublishToken is the bearer credential for the delivery endpoint
	// (non-stub only). It is never logged.
	PublishToken string
	// PublishIdempotencyField names the provider-specific header/query field
	// used for delivery idempotency (non-stub only). Optional: when empty, the
	// generic HTTP adapter derives a deterministic key but does not attach a
	// provider-specific field.
	PublishIdempotencyField string
	// PublishMaxAttempts bounds total delivery attempts (>= 1; non-stub only).
	// Zero uses the adapter's single-attempt default (no retry).
	PublishMaxAttempts int
	// PublishRetryBackoffBase/PublishRetryBackoffMax bound the retry backoff
	// pacing (non-stub only). Zero uses the adapter defaults.
	PublishRetryBackoffBase time.Duration
	PublishRetryBackoffMax  time.Duration
}

var globalConfig *Config

// ErrConfigInvalid is returned when required configuration is missing or set
// to an insecure placeholder/default. The error never exposes secret values.
var ErrConfigInvalid = errors.New("invalid configuration")

func defaults() *Config {
	usageLimit := uint64(0)
	usageLimitRaw := os.Getenv("AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE")
	if usageLimitRaw != "" {
		if v, err := strconv.ParseUint(usageLimitRaw, 10, 64); err == nil {
			usageLimit = v
		}
	}
	return &Config{
		ServerAddress:    getEnv("AUSTRO_SERVER_ADDR", "0.0.0.0:8080"),
		PostgresDSN:      getEnv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@localhost:5432/austro?sslmode=disable"),
		RedisAddr:        getEnv("AUSTRO_REDIS_ADDR", "localhost:6379"),
		RabbitMQURL:      getEnv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@localhost:5672"),
		RabbitMQQueue:    getEnv("AUSTRO_RABBITMQ_QUEUE", "austro.events"),
		JWTSecret:        getEnv("AUSTRO_JWT_SECRET", "change-me-in-production"),
		JWTRefreshSecret: getEnv("AUSTRO_JWT_REFRESH_SECRET", "change-me-in-production"),
		Environment:      getEnv("AUSTRO_ENV", "development"),
		WorkspaceID:      getEnv("AUSTRO_WORKSPACE_ID", "default"),

		AIBackend:                getEnv("AUSTRO_AI_BACKEND", AIBackendStub),
		AIModel:                  os.Getenv("AUSTRO_AI_MODEL"),
		AIBaseURL:                os.Getenv("AUSTRO_AI_BASE_URL"),
		AIAPIKey:                 os.Getenv("AUSTRO_AI_API_KEY"),
		AIUsageLimitPerWorkspace: usageLimit,
		usageLimitRaw:            usageLimitRaw,

		PublishBackend:          getEnv("AUSTRO_PUBLISH_BACKEND", PublishBackendStub),
		PublishWebhookURL:       os.Getenv("AUSTRO_PUBLISH_WEBHOOK_URL"),
		PublishToken:            os.Getenv("AUSTRO_PUBLISH_TOKEN"),
		PublishIdempotencyField: os.Getenv("AUSTRO_PUBLISH_IDEMPOTENCY_FIELD"),
		PublishMaxAttempts:      envInt("AUSTRO_PUBLISH_MAX_ATTEMPTS", 0),
		PublishRetryBackoffBase: envDuration("AUSTRO_PUBLISH_RETRY_BACKOFF_BASE", 0),
		PublishRetryBackoffMax:  envDuration("AUSTRO_PUBLISH_RETRY_BACKOFF_MAX", 0),
	}
}

// Load reads configuration from the environment, falling back to development
// defaults for callers that intentionally run outside a configured runtime
// (tests). Runtime entrypoints must use LoadStrict to fail fast.
func Load() *Config {
	globalConfig = defaults()
	return globalConfig
}

func Get() *Config {
	if globalConfig == nil {
		return Load()
	}
	return globalConfig
}

// LoadStrict reads configuration without insecure fallbacks and fails fast if
// any required setting is missing or set to a placeholder/default. It returns
// a structured error naming the affected settings (never their values).
func LoadStrict() (*Config, error) {
	cfg := defaults()
	return cfg, cfg.Validate()
}

// Validate fails fast on missing, empty, or insecure required settings. The
// returned error lists setting names only; no secret values are exposed.
func (c *Config) Validate() error {
	var missing []string
	for name, value := range map[string]string{
		"AUSTRO_POSTGRES_DSN":       c.PostgresDSN,
		"AUSTRO_REDIS_ADDR":         c.RedisAddr,
		"AUSTRO_RABBITMQ_URL":       c.RabbitMQURL,
		"AUSTRO_JWT_SECRET":         c.JWTSecret,
		"AUSTRO_JWT_REFRESH_SECRET": c.JWTRefreshSecret,
	} {
		if isInsecure(value) {
			missing = append(missing, name)
		}
	}

	missing = append(missing, c.validateBackends()...)

	if c.usageLimitRaw != "" {
		if _, err := strconv.ParseUint(c.usageLimitRaw, 10, 64); err != nil {
			missing = append(missing, "AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE")
		}
	}

	if c.PublishMaxAttempts < 0 {
		missing = append(missing, "AUSTRO_PUBLISH_MAX_ATTEMPTS")
	}
	if c.PublishRetryBackoffBase < 0 || c.PublishRetryBackoffMax < 0 {
		missing = append(missing, "AUSTRO_PUBLISH_RETRY_BACKOFF")
	}

	if len(missing) > 0 {
		return fmt.Errorf("%w: required settings missing or insecure: %s", ErrConfigInvalid, strings.Join(missing, ", "))
	}
	return nil
}

// validateBackends enforces the cross-field rules for the AI and publishing
// adapter selection. The deterministic stub adapters require no external
// configuration; selecting any real backend is CONFIGURATION_REQUIRED and its
// settings must be present and secure, or validation fails fast. Supplying
// external settings while the stub backend is selected is rejected so a
// configured credential is never silently ignored.
func (c *Config) validateBackends() []string {
	var missing []string

	switch c.AIBackend {
	case "", AIBackendStub:
		// Offline adapter: no external credential may be present.
		for name, value := range map[string]string{
			"AUSTRO_AI_MODEL":    c.AIModel,
			"AUSTRO_AI_BASE_URL": c.AIBaseURL,
			"AUSTRO_AI_API_KEY":  c.AIAPIKey,
		} {
			if value != "" {
				missing = append(missing, name)
			}
		}
	case AIBackendOpenAICompatible:
		for name, value := range map[string]string{
			"AUSTRO_AI_MODEL":    c.AIModel,
			"AUSTRO_AI_BASE_URL": c.AIBaseURL,
			"AUSTRO_AI_API_KEY":  c.AIAPIKey,
		} {
			if isInsecure(value) {
				missing = append(missing, name)
			}
		}
	default:
		missing = append(missing, "AUSTRO_AI_BACKEND")
	}

	switch c.PublishBackend {
	case "", PublishBackendStub:
		for name, value := range map[string]string{
			"AUSTRO_PUBLISH_WEBHOOK_URL": c.PublishWebhookURL,
			"AUSTRO_PUBLISH_TOKEN":       c.PublishToken,
		} {
			if value != "" {
				missing = append(missing, name)
			}
		}
	case PublishBackendGenericHTTP:
		for name, value := range map[string]string{
			"AUSTRO_PUBLISH_WEBHOOK_URL": c.PublishWebhookURL,
			"AUSTRO_PUBLISH_TOKEN":       c.PublishToken,
		} {
			if isInsecure(value) {
				missing = append(missing, name)
			}
		}
	default:
		missing = append(missing, "AUSTRO_PUBLISH_BACKEND")
	}

	return missing
}

// MustLoad is the runtime startup gate: it validates required configuration
// and, on any missing or insecure setting, emits a structured error and
// terminates instead of merely warning (fail-fast).
func MustLoad(cfg *Config) {
	if err := cfg.Validate(); err != nil {
		logger.NewEntry("invalid-configuration").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
}

// isInsecure reports whether a required setting is absent or still set to one
// of the development placeholders/defaults that must never be used at runtime.
func isInsecure(v string) bool {
	if v == "" {
		return true
	}
	lower := strings.ToLower(v)
	for _, bad := range []string{"change-me", "changeme", "localhost:5432", "localhost:6379", "localhost:5672", "example"} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// envInt reads an integer environment value, returning fallback for an empty
// or non-integer value. Validation (Validate) rejects a negative attempt count.
func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

// envDuration reads a Go duration string (e.g. "500ms", "30s") returning
// fallback for an empty or unparseable value.
func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return v
}
