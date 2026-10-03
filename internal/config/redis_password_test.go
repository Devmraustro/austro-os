package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// AUSTRO_REDIS_PASSWORD is the AUTH password presented to the Redis server.
// These tests pin the four behaviours that matter operationally: it is read,
// an empty value is a legitimate passwordless deployment, a placeholder is
// refused before startup, and a refusal never echoes the secret.

// redisAuthEnv sets the minimum environment LoadStrict needs, with the Redis
// password left unset so each test can decide it.
//
// The runtime and admin DSNs are cleared rather than left alone. LoadStrict
// validates a supplied runtime DSN and rejects a loopback endpoint unless
// AUSTRO_ALLOW_LOOPBACK is set, so an ambient runtime DSN pointing at a local
// verification instance would fail these tests for a reason that has nothing to
// do with the Redis password. Clearing them keeps the outcome a function of what
// the test sets.
func redisAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@db:5432/austro?sslmode=disable")
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@rabbitmq:5672")
	t.Setenv("AUSTRO_JWT_SECRET", "prod-access-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_JWT_REFRESH_SECRET", "prod-refresh-secret-1234567890-abcdef")
	t.Setenv("AUSTRO_POSTGRES_RUNTIME_DSN", "")
	t.Setenv("AUSTRO_POSTGRES_ADMIN_DSN", "")
	t.Setenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES", "")
}

// TestLoadStrictReadsRedisPasswordFromEnv proves the setting reaches Config.
func TestLoadStrictReadsRedisPasswordFromEnv(t *testing.T) {
	redisAuthEnv(t)
	t.Setenv("AUSTRO_REDIS_PASSWORD", "s3cr3t-redis-auth-9f8e7d6c")

	cfg, err := LoadStrict()
	require.NoError(t, err)
	require.Equal(t, "s3cr3t-redis-auth-9f8e7d6c", cfg.RedisPassword)
}

// TestLoadStrictAllowsEmptyRedisPassword proves an empty value is accepted.
// A Redis with no requirepass is a real deployment shape -- a loopback-only
// development instance, or a managed instance that authenticates at the network
// layer -- so treating the empty string as "misconfigured" would block a
// legitimate configuration.
func TestLoadStrictAllowsEmptyRedisPassword(t *testing.T) {
	redisAuthEnv(t)
	t.Setenv("AUSTRO_REDIS_PASSWORD", "")

	cfg, err := LoadStrict()
	require.NoError(t, err)
	require.Empty(t, cfg.RedisPassword)
}

// TestLoadStrictRejectsPlaceholderRedisPassword proves a copied-from-the-docs
// value fails startup instead of silently failing every AUTH command later.
func TestLoadStrictRejectsPlaceholderRedisPassword(t *testing.T) {
	for _, placeholder := range []string{
		"change-me",
		"change-me-in-production",
		"changeme",
		"example-redis-password",
		"CHANGEME-REDIS",
	} {
		t.Run(placeholder, func(t *testing.T) {
			redisAuthEnv(t)
			t.Setenv("AUSTRO_REDIS_PASSWORD", placeholder)

			_, err := LoadStrict()
			require.ErrorIs(t, err, ErrConfigInvalid)
			require.Contains(t, err.Error(), "AUSTRO_REDIS_PASSWORD")
		})
	}
}

// TestRedisPasswordRejectionDoesNotLeakTheValue is the reason the message names
// the setting rather than describing the value. An operator's most likely first
// instinct on a startup failure is to paste the error somewhere shared.
func TestRedisPasswordRejectionDoesNotLeakTheValue(t *testing.T) {
	redisAuthEnv(t)
	const secret = "my-unique-redis-passphrase-do-not-log-4f2b1c"
	// Contains a denylisted substring so validation rejects it.
	t.Setenv("AUSTRO_REDIS_PASSWORD", secret+"-changeme")

	_, err := LoadStrict()
	require.ErrorIs(t, err, ErrConfigInvalid)
	require.Contains(t, err.Error(), "AUSTRO_REDIS_PASSWORD")
	require.NotContains(t, err.Error(), secret,
		"a validation failure must never echo the secret it rejected")
}

// TestRedisPasswordIsIndependentOfRedisAddr proves the password is not smuggled
// through AUSTRO_REDIS_ADDR userinfo. ParseRedisAddr rejects credentials in the
// address, so a password carried there could never authenticate; keeping them
// separate is what makes the setting usable.
func TestRedisPasswordIsIndependentOfRedisAddr(t *testing.T) {
	redisAuthEnv(t)
	t.Setenv("AUSTRO_REDIS_ADDR", "redis:6379")
	t.Setenv("AUSTRO_REDIS_PASSWORD", "redispass-aaaaaaaaaaaaaaaa")

	cfg, err := LoadStrict()
	require.NoError(t, err)
	require.Equal(t, "redis:6379", cfg.RedisAddr)
	require.Equal(t, "redispass-aaaaaaaaaaaaaaaa", cfg.RedisPassword)
}
