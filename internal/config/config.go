package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	logger "austro-os/internal/log"
)

type Config struct {
	ServerAddress    string
	PostgresDSN      string
	RedisAddr        string
	RabbitMQURL      string
	JWTSecret        string
	JWTRefreshSecret string
	Environment      string
	WorkspaceID      string
}

var globalConfig *Config

// ErrConfigInvalid is returned when required configuration is missing or set
// to an insecure placeholder/default. The error never exposes secret values.
var ErrConfigInvalid = errors.New("invalid configuration")

func defaults() *Config {
	return &Config{
		ServerAddress:    getEnv("AUSTRO_SERVER_ADDR", "0.0.0.0:8080"),
		PostgresDSN:      getEnv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@localhost:5432/austro?sslmode=disable"),
		RedisAddr:        getEnv("AUSTRO_REDIS_ADDR", "localhost:6379"),
		RabbitMQURL:      getEnv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@localhost:5672"),
		JWTSecret:        getEnv("AUSTRO_JWT_SECRET", "change-me-in-production"),
		JWTRefreshSecret: getEnv("AUSTRO_JWT_REFRESH_SECRET", "change-me-in-production"),
		Environment:      getEnv("AUSTRO_ENV", "development"),
		WorkspaceID:      getEnv("AUSTRO_WORKSPACE_ID", "default"),
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
	if len(missing) > 0 {
		return fmt.Errorf("%w: required settings missing or insecure: %s", ErrConfigInvalid, strings.Join(missing, ", "))
	}
	return nil
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