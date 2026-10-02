package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsLoopbackEndpointDetectsEveryWrittenForm is the regression test for the
// check that used to compare the whole value against the literal strings
// "localhost:5432", "localhost:6379" and "localhost:5672".
//
// That matched "localhost:6379" and nothing else, so "127.0.0.1:6379" - the
// form in every example file and every CI workflow in this repository - passed
// validation. The documented promise that the address "must not resolve to a
// loopback address" was kept for one spelling of it and quietly broken for the
// other.
func TestIsLoopbackEndpointDetectsEveryWrittenForm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{"IPv4 loopback literal", "127.0.0.1:6379", true},
		{"IPv4 loopback in the 127/8 range", "127.0.0.53:6379", true},
		{"IPv6 loopback literal", "[::1]:6379", true},
		{"the name", "localhost:6379", true},
		{"the name in another case", "LOCALHOST:6379", true},
		{"the unspecified address is this host too", "0.0.0.0:6379", true},
		{"the unspecified IPv6 address", "[::]:6379", true},
		{"IPv4 loopback inside a PostgreSQL DSN", "postgres://austro:austro@127.0.0.1:5432/austro?sslmode=disable", true},
		{"localhost inside a PostgreSQL DSN", "postgres://austro:austro@localhost:5432/austro", true},
		{"IPv4 loopback inside an AMQP URL", "amqp://austro:austro@127.0.0.1:5672", true},
		{"IPv6 loopback inside a URL", "rediss://[::1]:6379", true},

		{"a Compose service name", "redis:6379", false},
		{"an internal hostname", "cache.internal:6379", false},
		{"a routable address", "10.0.0.5:6379", false},
		{"a public address", "203.0.113.10:6379", false},
		{"a routable PostgreSQL DSN", "postgres://austro:austro@db.internal:5432/austro", false},
		{"a routable AMQP URL", "amqp://austro:austro@rabbitmq:5672", false},
		{"empty", "", false},
		{"unparsable", "://", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isLoopbackEndpoint(tc.value),
				"isLoopbackEndpoint(%q)", tc.value)
		})
	}
}

// TestIsLoopbackEndpointIsNotAppliedToSecrets records why the loopback question
// is a separate check rather than another entry in isInsecure: isInsecure asks
// whether a value is a placeholder and runs over every setting, including
// secrets, where "localhost" is not evidence of anything.
func TestLoopbackCheckIsSeparateFromThePlaceholderCheck(t *testing.T) {
	require.True(t, isInsecure("change-me-in-production"), "placeholders are isInsecure's job")
	require.False(t, isInsecure("redis:6379"), "and isInsecure is not where endpoints are judged")
}

// TestAllowLoopbackOptInIsStrict asserts the opt-out cannot be enabled by a
// plausible typo.
//
// The failure this replaces was silent acceptance; an opt-out that a typo
// silently enables would restore exactly that, one keystroke away.
func TestAllowLoopbackOptInIsStrict(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "True", "yes", "YES", " 1 ", " true "} {
		t.Run("enabled by "+value, func(t *testing.T) {
			t.Setenv(allowLoopbackEnv, value)
			require.True(t, loopbackAllowed(), "%q is an affirmative spelling", value)
		})
	}

	for _, value := range []string{"", "0", "false", "no", "off", "sure", "please", "enabled", "y", "t", "loopback"} {
		t.Run("not enabled by "+value, func(t *testing.T) {
			t.Setenv(allowLoopbackEnv, value)
			require.False(t, loopbackAllowed(), "%q is not an affirmative spelling and must leave the check in force", value)
		})
	}
}
