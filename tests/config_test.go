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

// ---------------------------------------------------------------------------
// Pre-provisioned roles (AUSTRO_POSTGRES_PREPROVISIONED_ROLES)
//
// The topology below mirrors a real AlwaysData deployment: the platform
// pre-created the roles austro (owner), austro_app (runtime) and
// austro_app_admin (admin) on postgresql-austro.alwaysdata.net, database
// austro_os, and the application connects to all three with dedicated
// credentials.
// ---------------------------------------------------------------------------

const (
	ppOwnerDSN   = "postgres://austro:pp-owner-cred-1234@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require"
	ppRuntimeDSN = "postgres://austro_app:pp-runtime-cred-5678@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require"
	ppAdminDSN   = "postgres://austro_app_admin:pp-admin-cred-9012@postgresql-austro.alwaysdata.net:5432/austro_os?sslmode=require"
)

// ppEnv states the pre-provisioned topology through the environment and
// neutralises every related setting the scenario does not name, so each test
// sees exactly the configuration it declares.
func ppEnv(t *testing.T, flag, ownerDSN, runtimeDSN, adminDSN string) {
	t.Helper()
	t.Setenv("AUSTRO_POSTGRES_DSN", ownerDSN)
	t.Setenv("AUSTRO_POSTGRES_RUNTIME_DSN", runtimeDSN)
	t.Setenv("AUSTRO_POSTGRES_ADMIN_DSN", adminDSN)
	t.Setenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES", flag)
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")
}

// TestPreProvisionedConfigValid verifies a complete pre-provisioned topology
// (owner + explicit runtime + explicit admin DSN) passes the strict loader,
// and that the explicit DSNs are carried through verbatim rather than
// re-derived.
func TestPreProvisionedConfigValid(t *testing.T) {
	ppEnv(t, "true", ppOwnerDSN, ppRuntimeDSN, ppAdminDSN)
	cfg, err := config.LoadStrict()
	require.NoError(t, err)
	require.True(t, cfg.PostgresPreProvisionedRoles)
	require.Equal(t, ppRuntimeDSN, cfg.PostgresRuntimeDSN,
		"the explicit runtime DSN must be used as supplied, not derived")
	require.Equal(t, ppAdminDSN, cfg.PostgresAdminDSN,
		"the dedicated admin DSN must be used as supplied, not derived")
}

// TestPreProvisionedMissingRuntimeDSN verifies pre-provisioned mode without an
// explicit runtime DSN fails fast: deriving one from the owner would be a
// credential the server cannot authenticate.
func TestPreProvisionedMissingRuntimeDSN(t *testing.T) {
	ppEnv(t, "true", ppOwnerDSN, "", ppAdminDSN)
	_, err := config.LoadStrict()
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_RUNTIME_DSN")
}

// TestPreProvisionedMissingAdminDSN verifies pre-provisioned mode without an
// explicit admin DSN fails fast: the platform created the admin role with its
// own credential, and a derived DSN would not authenticate as it.
func TestPreProvisionedMissingAdminDSN(t *testing.T) {
	ppEnv(t, "true", ppOwnerDSN, ppRuntimeDSN, "")
	_, err := config.LoadStrict()
	require.Error(t, err)
	require.ErrorIs(t, err, config.ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_ADMIN_DSN")
}

// TestPreProvisionedRuntimeEqualsOwner verifies the runtime principal is
// rejected when it collapses onto the schema owner, in pre-provisioned mode as
// in any other.
func TestPreProvisionedRuntimeEqualsOwner(t *testing.T) {
	ppEnv(t, "true", ppOwnerDSN, ppOwnerDSN, ppAdminDSN)
	_, err := config.LoadStrict()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_RUNTIME_DSN")
}

// TestPreProvisionedAdminEqualsOwner verifies the admin principal is rejected
// when it collapses onto the schema owner.
func TestPreProvisionedAdminEqualsOwner(t *testing.T) {
	ppEnv(t, "true", ppOwnerDSN, ppRuntimeDSN, ppOwnerDSN)
	_, err := config.LoadStrict()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_ADMIN_DSN")
}

// TestPreProvisionedAdminEqualsRuntime verifies the admin principal is
// rejected when it collapses onto the application runtime role.
func TestPreProvisionedAdminEqualsRuntime(t *testing.T) {
	ppEnv(t, "true", ppOwnerDSN, ppRuntimeDSN, ppRuntimeDSN)
	_, err := config.LoadStrict()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_ADMIN_DSN")
}

// TestPreProvisionedInvalidFlagRejected verifies a flag value that does not
// parse as a boolean is a configuration error (fail-fast), not a silent "off".
func TestPreProvisionedInvalidFlagRejected(t *testing.T) {
	ppEnv(t, "maybe", ppOwnerDSN, ppRuntimeDSN, ppAdminDSN)
	_, err := config.LoadStrict()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_POSTGRES_PREPROVISIONED_ROLES")
}

// TestNormalModeUnchangedByPreProvisionedSetting verifies that with the flag
// unset or false the classic self-provisioned derivation is exactly what the
// process gets: the runtime DSN derived from the owner, no admin DSN at all,
// and the flag reported off.
func TestNormalModeUnchangedByPreProvisionedSetting(t *testing.T) {
	for _, flag := range []string{"", "false"} {
		t.Run("flag="+flag, func(t *testing.T) {
			ppEnv(t, flag, ppOwnerDSN, "", "")
			cfg, err := config.LoadStrict()
			require.NoError(t, err)
			require.False(t, cfg.PostgresPreProvisionedRoles)
			require.Equal(t, config.DeriveRuntimeDSN(ppOwnerDSN), cfg.PostgresRuntimeDSN,
				"normal mode must keep deriving the runtime DSN from the owner")
			require.Empty(t, cfg.PostgresAdminDSN)
		})
	}
}

// TestDedicatedAdminDSNSupportedInNormalMode verifies an explicitly supplied
// admin DSN is honored (not replaced by a derivation) when the flag is off.
func TestDedicatedAdminDSNSupportedInNormalMode(t *testing.T) {
	ppEnv(t, "", ppOwnerDSN, ppRuntimeDSN, ppAdminDSN)
	cfg, err := config.LoadStrict()
	require.NoError(t, err)
	require.False(t, cfg.PostgresPreProvisionedRoles)
	require.Equal(t, ppAdminDSN, cfg.PostgresAdminDSN)
}
