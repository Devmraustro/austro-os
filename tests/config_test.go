package austro_os_test

import (
	"testing"

	"austro-os/internal/config"

	"github.com/stretchr/testify/require"
)

// TestConfigFailFastMissingRequired verifies that a Config with absent required
// settings is rejected by Validate.
func TestConfigFailFastMissingRequired(t *testing.T) {
	cfg := &config.Config{}
	err := cfg.Validate()
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrConfigInvalid)
}

// TestConfigStrictMissingProductionEnv verifies that LoadStrict fails when the
// runtime environment does not supply secure required settings. The default
// fallbacks are development placeholders and must be rejected.
func TestConfigStrictMissingProductionEnv(t *testing.T) {
	// Unset all required settings so LoadStrict falls back to insecure
	// development defaults, which fail-fast validation must reject.
	t.Setenv("AUSTRO_POSTGRES_DSN", "")
	t.Setenv("AUSTRO_REDIS_ADDR", "")
	t.Setenv("AUSTRO_RABBITMQ_URL", "")
	t.Setenv("AUSTRO_JWT_SECRET", "")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "")

	_, err := config.LoadStrict()
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrConfigInvalid)
}

// TestConfigInsecureJWTSecretRejected verifies the documented production
// placeholder for the JWT secrets is treated as insecure and rejected.
func TestConfigInsecureJWTSecretRejected(t *testing.T) {
	cfg := &config.Config{
		PostgresDSN:      "postgres://austro:austro@db:5432/austro?sslmode=disable",
		RedisAddr:        "redis:6379",
		RabbitMQURL:      "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:        "change-me-in-production",
		JWTRefreshSecret: "change-me-in-production",
	}
	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_JWT_SECRET")
	require.Contains(t, err.Error(), "AUSTRO_JWT_REFRESH_SECRET")
}

// TestConfigMissingDatabaseRejected verifies a missing/insecure database
// connection setting alone is enough to fail validation.
func TestConfigMissingDatabaseRejected(t *testing.T) {
	cfg := &config.Config{
		PostgresDSN:      "",
		RedisAddr:        "redis:6379",
		RabbitMQURL:      "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:        "prod-access-secret-1234567890-abcdef",
		JWTRefreshSecret: "prod-refresh-secret-1234567890-abcdef",
	}
	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_DSN")
}

// TestConfigInvalidValueRejected verifies a malformed/insecure setting (a
// default-with-localhost host) yields a validation error.
func TestConfigInvalidValueRejected(t *testing.T) {
	cfg := &config.Config{
		PostgresDSN:      "postgres://austro:austro@localhost:5432/austro?sslmode=disable",
		RedisAddr:        "redis:6379",
		RabbitMQURL:      "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:        "prod-access-secret-1234567890-abcdef",
		JWTRefreshSecret: "prod-refresh-secret-1234567890-abcdef",
	}
	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_DSN")
}

// TestConfigValidPasses verifies a fully secure configuration is accepted by
// both Validate and LoadStrict.
func TestConfigValidPasses(t *testing.T) {
	cfg := &config.Config{
		PostgresDSN:      "postgres://austro:austro@db:5432/austro?sslmode=disable",
		RedisAddr:        "redis:6379",
		RabbitMQURL:      "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:        "prod-access-secret-1234567890-abcdef",
		JWTRefreshSecret: "prod-refresh-secret-1234567890-abcdef",
	}
	require.NoError(t, cfg.Validate())

	// The same secure values supplied through the environment must also pass
	// the strict loader.
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")

	loaded, err := config.LoadStrict()
	require.NoError(t, err)
	require.NotNil(t, loaded)
}

// TestConfigErrorDoesNotExposeSecrets verifies that validation reports which
// setting is insecure without echoing the actual secret value into the error.
func TestConfigErrorDoesNotExposeSecrets(t *testing.T) {
	// The JWT secret embeds the "change-me" placeholder (making it insecure and
	// therefore flagged) while also carrying a unique portion that must never
	// be echoed back into a validation error.
	secret := "change-me-s3cr3t-ACCESS-KEY-9f8e7d6c5b4a-unique"
	cfg := &config.Config{
		PostgresDSN:      "postgres://austro:austro@db:5432/austro?sslmode=disable",
		RedisAddr:        "redis:6379",
		RabbitMQURL:      "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:        secret,
		JWTRefreshSecret: "prod-refresh-secret-1234567890-abcdef",
	}
	err := cfg.Validate()
	require.Error(t, err)
	// The unique portion of the secret value must never leak into the error.
	require.NotContains(t, err.Error(), "s3cr3t")
	require.NotContains(t, err.Error(), "ACCESS-KEY")
	require.NotContains(t, err.Error(), "9f8e7d6c5b4a-unique")
	// The error must still identify the offending setting by name.
	require.Contains(t, err.Error(), "AUSTRO_JWT_SECRET")
	require.NotContains(t, err.Error(), cfg.JWTSecret)
}

// TestConfigErrorUsesSettingNames verifies the error is deterministic and
// refers to settings by their environment variable names, never values.
func TestConfigErrorUsesSettingNames(t *testing.T) {
	cfg := &config.Config{}
	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_DSN")
	require.Contains(t, err.Error(), "AUSTRO_REDIS_ADDR")
	require.Contains(t, err.Error(), "AUSTRO_RABBITMQ_URL")
	require.Contains(t, err.Error(), "AUSTRO_JWT_SECRET")
	require.Contains(t, err.Error(), "AUSTRO_JWT_REFRESH_SECRET")
}
