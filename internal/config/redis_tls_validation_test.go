package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateAcceptsATLSRedisAddressWithAPrivateCA is the configuration a
// managed Redis behind a private certificate authority needs: one address that
// states the transport is encrypted, and the authority that issued the
// certificate.
func TestValidateAcceptsATLSRedisAddressWithAPrivateCA(t *testing.T) {
	cfg := secureCore()
	cfg.RedisAddr = "rediss://redis.managed-host.net:6379"
	cfg.RedisCAFile = "/etc/austro/redis-ca.pem"
	require.NoError(t, cfg.Validate())
}

// TestValidateAcceptsATLSRedisAddressWithoutAPrivateCA covers a managed service
// whose certificate chains to a public authority, which needs no bundle.
func TestValidateAcceptsATLSRedisAddressWithoutAPrivateCA(t *testing.T) {
	cfg := secureCore()
	cfg.RedisAddr = "rediss://redis.managed-host.net:6379"
	require.NoError(t, cfg.Validate())
}

// TestValidateRejectsACAFileWithoutTLS asserts a CA bundle supplied for a
// plaintext connection is refused.
//
// Silently accepting it would be the worst outcome: the operator would believe
// they had configured verification, and the connection would carry credentials
// in the clear. The bundle can never be used without rediss://, so the
// combination can only be a mistake.
func TestValidateRejectsACAFileWithoutTLS(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
	}{
		{"legacy bare address", "redis:6379"},
		{"explicit plaintext", "redis://cache.eu.internal:6379"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := secureCore()
			cfg.RedisAddr = tc.addr
			cfg.RedisCAFile = "/etc/austro/redis-ca.pem"

			err := cfg.Validate()
			require.ErrorIs(t, err, ErrConfigInvalid)
			require.Contains(t, err.Error(), "AUSTRO_REDIS_CA_FILE")
			require.Contains(t, err.Error(), "rediss://", "the error must say what the setting requires")
		})
	}
}

// TestValidateRejectsAMalformedRedisAddress asserts a bad address is reported
// as configuration, naming the accepted forms, rather than reaching the client
// and surfacing as an opaque network error at startup.
func TestValidateRejectsAMalformedRedisAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
	}{
		{"no port", "cache.eu.internal"},
		{"unsupported scheme", "https://cache.eu.internal:6379"},
		{"port out of range", "cache.eu.internal:70000"},
		{"credentials that would be dropped", "rediss://austro:hunter2@cache.eu.internal:6379"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := secureCore()
			cfg.RedisAddr = tc.addr

			err := cfg.Validate()
			require.ErrorIs(t, err, ErrConfigInvalid)
			require.Contains(t, err.Error(), "AUSTRO_REDIS_ADDR")
			require.Contains(t, err.Error(), "rediss://", "the error must name the forms that are accepted")
			require.NotContains(t, err.Error(), "hunter2", "a configuration error must not print a password")
		})
	}
}

// TestValidateRejectsLoopbackEndpointsUnlessDeclared is the end-to-end form of
// the false negative: an address the old check missed now fails validation, and
// can only be used by declaring it.
func TestValidateRejectsLoopbackEndpointsUnlessDeclared(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*Config)
		settings string
	}{
		{
			name:     "Redis by IPv4 literal",
			mutate:   func(c *Config) { c.RedisAddr = "127.0.0.1:6379" },
			settings: "AUSTRO_REDIS_ADDR",
		},
		{
			name:     "PostgreSQL by IPv4 literal",
			mutate:   func(c *Config) { c.PostgresDSN = "postgres://austro:austro@127.0.0.1:5432/austro?sslmode=disable" },
			settings: "AUSTRO_POSTGRES_DSN",
		},
		{
			name:     "RabbitMQ by IPv4 literal",
			mutate:   func(c *Config) { c.RabbitMQURL = "amqp://austro:austro@127.0.0.1:5672" },
			settings: "AUSTRO_RABBITMQ_URL",
		},
		{
			name: "the runtime DSN by IPv4 literal",
			mutate: func(c *Config) {
				c.PostgresRuntimeDSN = "postgres://austro_app:pw@127.0.0.1:5432/austro?sslmode=disable"
			},
			settings: "AUSTRO_POSTGRES_RUNTIME_DSN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := secureCore()
			tc.mutate(cfg)

			err := cfg.Validate()
			require.ErrorIs(t, err, ErrConfigInvalid, "a loopback endpoint must not pass validation by accident")
			require.Contains(t, err.Error(), tc.settings)

			t.Setenv(allowLoopbackEnv, "1")
			require.NoError(t, cfg.Validate(),
				"a deployment that genuinely dials loopback must be able to say so")
		})
	}
}

// TestAllowLoopbackNearMissValuesDoNotDisableTheCheck asserts the end-to-end
// consequence of the strict opt-in: a mistyped acknowledgement does not quietly
// restore the false negative.
func TestAllowLoopbackNearMissValuesDoNotDisableTheCheck(t *testing.T) {
	for _, value := range []string{"sure", "y", "true-ish", "0", ""} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(allowLoopbackEnv, value)

			cfg := secureCore()
			cfg.RedisAddr = "127.0.0.1:6379"

			err := cfg.Validate()
			require.ErrorIs(t, err, ErrConfigInvalid)
			require.Contains(t, err.Error(), "AUSTRO_REDIS_ADDR")
		})
	}
}

// TestValidateOnlyRequiresRedisFromTheRoleThatDialsIt confirms the TLS and
// loopback rules did not widen the Redis dependency to the roles that never
// open the connection. A dispatcher with no Redis configured at all must still
// validate, and an address it does not use must not be judged.
func TestValidateDoesNotRequireRedisFromRolesThatDoNotDialIt(t *testing.T) {
	cfg := secureCore()
	cfg.ProcessRole = ProcessRoleDispatcher
	cfg.ExecutionDispatchMode = ExecutionDispatchModeService
	cfg.RedisAddr = "127.0.0.1:6379"

	require.NoError(t, cfg.Validate(),
		"the dispatcher never dials Redis, so its Redis address must not be judged")
}
