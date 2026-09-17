package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
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
	// AIBackendLocal selects a LOCAL/free OpenAI-compatible endpoint (for
	// example a locally hosted model server or an in-house inference box).
	// Unlike openai-compatible, it does not require an API key: local
	// endpoints often need no credentials. Model and BaseURL are still
	// CONFIGURATION_REQUIRED. No block: the project must run without any paid
	// provider, and with no key where the operator's endpoint needs none.
	AIBackendLocal = "local"
	// AIBackendOpenAICompatible is a real OpenAI-compatible HTTP endpoint.
	// An API key is CONFIGURATION_REQUIRED; providers offering a free tier are
	// configured here later by the operator (never hardcoded).
	AIBackendOpenAICompatible = "openai-compatible"
	// PublishBackendStub is the deterministic, offline publishing adapter (default).
	PublishBackendStub = "stub"
	// PublishBackendGenericHTTP is a real generic HTTP delivery endpoint.
	PublishBackendGenericHTTP = "generic-http"
)

type Config struct {
	ServerAddress string
	PostgresDSN   string

	// PostgresRuntimeDSN is the connection the application actually serves
	// traffic on. It must resolve to a role that is NOT a superuser, does NOT
	// have BYPASSRLS, and does NOT own the workspace-scoped tables, so the
	// row level security policies genuinely constrain it. PostgresDSN, by
	// contrast, is the schema owner used only to bootstrap and migrate.
	//
	// Leaving it empty derives a runtime DSN from PostgresDSN (same host,
	// database and password, role AUSTRO_POSTGRES_RUNTIME_USER or
	// "austro_app"), which is what keeps a developer running with no extra
	// configuration still on the unprivileged path: infrastructure/database
	// provisions the role and then verifies the live connection is
	// unprivileged before serving a single request. Deployments set it
	// explicitly so the runtime role has its own credential.
	PostgresRuntimeDSN string

	RedisAddr        string
	RabbitMQURL      string
	RabbitMQQueue    string
	JWTSecret        string
	JWTRefreshSecret string
	Environment      string
	WorkspaceID      string

	// FounderUsername/FounderPassword configure the single founder identity
	// (AUSTRO_FOUNDER_USERNAME / AUSTRO_FOUNDER_PASSWORD). Both must be set
	// together; until then the bootstrap endpoint is disabled (409). The values
	// are deployment configuration, never hardcoded, and the password is never
	// logged or echoed.
	FounderUsername string
	FounderPassword string

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

	// Raw environment text for the settings above. Strict validation rejects a
	// value that does not parse rather than silently substituting the default,
	// because a typo in a retry budget would otherwise be indistinguishable
	// from an intentionally unset one.
	publishMaxAttemptsRaw string
	publishBackoffBaseRaw string
	publishBackoffMaxRaw  string
}

// Minimum accepted lengths for credentials.
const (
	// minHMACKeyBytes is the floor for the JWT signing secrets. RFC 7518 §3.2
	// requires a key used with a MAC algorithm to be at least as long as the
	// hash output, so HS256 needs 32 bytes; a shorter key is below the
	// algorithm's design strength regardless of how random it looks.
	minHMACKeyBytes = 32
	// minFounderPasswordBytes is the floor for the single founder credential.
	// It is the only interactive credential in the system and gates
	// organization-level authority, so it is held to a password floor rather
	// than a key floor.
	minFounderPasswordBytes = 16
)

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
	ownerDSN := getEnv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@localhost:5432/austro?sslmode=disable")
	return &Config{
		ServerAddress: getEnv("AUSTRO_SERVER_ADDR", "0.0.0.0:8080"),
		PostgresDSN:   ownerDSN,
		// Derived rather than defaulted to a constant so there is exactly one
		// place a deployment states its database location, and so forgetting
		// the runtime DSN lands on the unprivileged role instead of quietly
		// serving traffic as the schema owner.
		PostgresRuntimeDSN: getEnv("AUSTRO_POSTGRES_RUNTIME_DSN", DeriveRuntimeDSN(ownerDSN)),
		RedisAddr:          getEnv("AUSTRO_REDIS_ADDR", "localhost:6379"),
		RabbitMQURL:        getEnv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@localhost:5672"),
		RabbitMQQueue:      getEnv("AUSTRO_RABBITMQ_QUEUE", "austro.events"),
		JWTSecret:          getEnv("AUSTRO_JWT_SECRET", "change-me-in-production"),
		JWTRefreshSecret:   getEnv("AUSTRO_JWT_REFRESH_SECRET", "change-me-in-production"),
		Environment:        getEnv("AUSTRO_ENV", "development"),
		WorkspaceID:        getEnv("AUSTRO_WORKSPACE_ID", "default"),

		FounderUsername: os.Getenv("AUSTRO_FOUNDER_USERNAME"),
		FounderPassword: os.Getenv("AUSTRO_FOUNDER_PASSWORD"),

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

		publishMaxAttemptsRaw: os.Getenv("AUSTRO_PUBLISH_MAX_ATTEMPTS"),
		publishBackoffBaseRaw: os.Getenv("AUSTRO_PUBLISH_RETRY_BACKOFF_BASE"),
		publishBackoffMaxRaw:  os.Getenv("AUSTRO_PUBLISH_RETRY_BACKOFF_MAX"),
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
		"AUSTRO_POSTGRES_DSN": c.PostgresDSN,
		"AUSTRO_REDIS_ADDR":   c.RedisAddr,
		"AUSTRO_RABBITMQ_URL": c.RabbitMQURL,
	} {
		if isInsecure(value) {
			missing = append(missing, name)
		}
	}

	// The runtime DSN is optional (it is derived from the owner DSN when
	// absent), so it is only checked when explicitly supplied. Two rules
	// apply: it must not be a placeholder, and it must not be the owner DSN.
	// Running application traffic as the schema owner is the exact defect
	// this setting exists to prevent, and PostgreSQL silently skips row level
	// security for a table's owner, so an owner connection would look
	// correctly configured while enforcing nothing.
	if c.PostgresRuntimeDSN != "" {
		if isInsecure(c.PostgresRuntimeDSN) {
			missing = append(missing, "AUSTRO_POSTGRES_RUNTIME_DSN")
		}
		if c.PostgresRuntimeDSN == c.PostgresDSN {
			missing = append(missing, "AUSTRO_POSTGRES_RUNTIME_DSN")
		}
	}

	// The JWT secrets carry an additional floor: a placeholder check alone
	// would accept a one-byte secret, which is below the design strength of
	// HS256 (RFC 7518 §3.2 requires the key to be at least the hash length).
	for name, value := range map[string]string{
		"AUSTRO_JWT_SECRET":         c.JWTSecret,
		"AUSTRO_JWT_REFRESH_SECRET": c.JWTRefreshSecret,
	} {
		if isInsecure(value) || len(value) < minHMACKeyBytes {
			missing = append(missing, name)
		}
	}

	missing = append(missing, c.validateBackends()...)

	if c.usageLimitRaw != "" {
		if _, err := strconv.ParseUint(c.usageLimitRaw, 10, 64); err != nil {
			missing = append(missing, "AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE")
		}
	}

	// A value that was supplied but does not parse is a configuration error,
	// not a request to use the default. envInt/envDuration fall back so the
	// value is never half-applied; validation is what turns that into a
	// fail-fast.
	for name, raw := range map[string]string{
		"AUSTRO_PUBLISH_MAX_ATTEMPTS":       c.publishMaxAttemptsRaw,
		"AUSTRO_PUBLISH_RETRY_BACKOFF_BASE": c.publishBackoffBaseRaw,
		"AUSTRO_PUBLISH_RETRY_BACKOFF_MAX":  c.publishBackoffMaxRaw,
	} {
		if raw == "" {
			continue
		}
		if name == "AUSTRO_PUBLISH_MAX_ATTEMPTS" {
			if _, err := strconv.Atoi(raw); err != nil {
				missing = append(missing, name)
			}
			continue
		}
		if _, err := time.ParseDuration(raw); err != nil {
			missing = append(missing, name)
		}
	}

	if c.PublishMaxAttempts < 0 {
		missing = append(missing, "AUSTRO_PUBLISH_MAX_ATTEMPTS")
	}
	if c.PublishRetryBackoffBase < 0 || c.PublishRetryBackoffMax < 0 {
		missing = append(missing, "AUSTRO_PUBLISH_RETRY_BACKOFF")
	}

	// Founder bootstrap is optional, but both settings must be supplied
	// together (a half-configured bootstrap would fail at runtime), and a
	// configured password must never be a placeholder or shorter than the
	// password floor.
	if (c.FounderUsername == "") != (c.FounderPassword == "") {
		missing = append(missing, "AUSTRO_FOUNDER_USERNAME/AUSTRO_FOUNDER_PASSWORD")
	}
	if c.FounderPassword != "" && (isInsecure(c.FounderPassword) || len(c.FounderPassword) < minFounderPasswordBytes) {
		missing = append(missing, "AUSTRO_FOUNDER_PASSWORD")
	}

	if len(missing) > 0 {
		// Sorted and de-duplicated so the same misconfiguration always produces
		// the same message instead of one in random map order.
		return fmt.Errorf("%w: required settings missing or insecure: %s", ErrConfigInvalid, strings.Join(sortedUnique(missing), ", "))
	}
	return nil
}

// sortedUnique returns names in stable order with repeats collapsed. Several
// rules can flag the same setting, and the error should name it once.
func sortedUnique(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
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
	case AIBackendLocal:
		// Local/free keyless OpenAI-compatible endpoint. Model and BaseURL are
		// CONFIGURATION_REQUIRED; the API key is optional (local endpoints often
		// need no credential). A supplied key must still be secure, and a
		// placeholder is rejected the same way it is for openai-compatible.
		for name, value := range map[string]string{
			"AUSTRO_AI_MODEL":    c.AIModel,
			"AUSTRO_AI_BASE_URL": c.AIBaseURL,
		} {
			if isInsecure(value) {
				missing = append(missing, name)
			}
		}
		if c.AIAPIKey != "" && isInsecure(c.AIAPIKey) {
			missing = append(missing, "AUSTRO_AI_API_KEY")
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
// DeriveRuntimeDSN rewrites an owner DSN into the unprivileged application
// runtime DSN: same host, port, database and query options, with the role
// replaced by AUSTRO_POSTGRES_RUNTIME_USER (default "austro_app") and the
// password replaced by AUSTRO_POSTGRES_RUNTIME_PASSWORD when that is set.
//
// A DSN that cannot be parsed yields an empty string rather than a guess:
// Validate then rejects the configuration instead of the process silently
// connecting as the owner.
func DeriveRuntimeDSN(ownerDSN string) string {
	u, err := url.Parse(ownerDSN)
	if err != nil || u.User == nil {
		return ""
	}
	user := getEnv("AUSTRO_POSTGRES_RUNTIME_USER", "austro_app")
	password, hasPassword := u.User.Password()
	if override := os.Getenv("AUSTRO_POSTGRES_RUNTIME_PASSWORD"); override != "" {
		password, hasPassword = override, true
	}
	if hasPassword {
		u.User = url.UserPassword(user, password)
	} else {
		u.User = url.User(user)
	}
	return u.String()
}

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
