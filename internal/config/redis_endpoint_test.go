package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseRedisAddrAcceptsExactlyThreeForms pins the accepted forms and, for
// each, the host the client will dial and whether the connection is encrypted.
//
// AUSTRO_REDIS_ADDR is the whole connection configuration, so this function is
// the only thing that decides whether a Redis connection is private. Before it
// existed the client had no way to request TLS at all, which made a managed
// Redis that requires TLS unreachable by this application.
func TestParseRedisAddrAcceptsExactlyThreeForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		want RedisEndpoint
	}{
		{
			name: "legacy bare address stays plaintext",
			addr: "redis:6379",
			want: RedisEndpoint{Addr: "redis:6379", Host: "redis"},
		},
		{
			name: "legacy bare address to another host",
			addr: "cache.eu.internal:6380",
			want: RedisEndpoint{Addr: "cache.eu.internal:6380", Host: "cache.eu.internal"},
		},
		{
			name: "redis scheme is explicit plaintext",
			addr: "redis://cache.eu.internal:6379",
			want: RedisEndpoint{Addr: "cache.eu.internal:6379", Host: "cache.eu.internal"},
		},
		{
			name: "rediss scheme requires TLS",
			addr: "rediss://cache.eu.internal:6379",
			want: RedisEndpoint{Addr: "cache.eu.internal:6379", Host: "cache.eu.internal", TLS: true},
		},
		{
			name: "the TLS scheme is matched case-insensitively",
			addr: "REDISS://cache.eu.internal:6379",
			want: RedisEndpoint{Addr: "cache.eu.internal:6379", Host: "cache.eu.internal", TLS: true},
		},
		{
			name: "TLS on the non-default port a managed service uses",
			addr: "rediss://redis.managed-host.net:16379",
			want: RedisEndpoint{Addr: "redis.managed-host.net:16379", Host: "redis.managed-host.net", TLS: true},
		},
		{
			name: "bracketed IPv6 literal without a scheme",
			addr: "[::1]:6379",
			want: RedisEndpoint{Addr: "[::1]:6379", Host: "::1"},
		},
		{
			name: "bracketed IPv6 literal with TLS",
			addr: "rediss://[2001:db8::1]:6379",
			want: RedisEndpoint{Addr: "[2001:db8::1]:6379", Host: "2001:db8::1", TLS: true},
		},
		{
			name: "surrounding whitespace is not significant",
			addr: "  rediss://cache.eu.internal:6379  ",
			want: RedisEndpoint{Addr: "cache.eu.internal:6379", Host: "cache.eu.internal", TLS: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRedisAddr(tc.addr)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestParseRedisAddrDoesNotMistakeAHostNamedRedisForAScheme is the trap this
// parser is most likely to fall into.
//
// "redis:6379" is the value used throughout the Compose files, the CI workflows
// and the test fixtures, and it means a host that happens to be called "redis".
// A parser that split on the first colon would read "redis" as a scheme and the
// host as empty, breaking every existing deployment the moment TLS support was
// added. The scheme is therefore recognized only when followed by "://".
func TestParseRedisAddrDoesNotMistakeAHostNamedRedisForAScheme(t *testing.T) {
	got, err := ParseRedisAddr("redis:6379")
	require.NoError(t, err, "the legacy form must keep working: it is what every existing deployment configures")
	require.Equal(t, "redis", got.Host)
	require.Equal(t, "redis:6379", got.Addr)
	require.False(t, got.TLS, "a bare address must not silently acquire TLS, which would break a plaintext deployment")
}

// TestParseRedisAddrRejectsInvalidConfigurations covers the values an operator
// can mistype, and asserts each is rejected with a reason rather than accepted
// and left to fail as an opaque dial error at startup.
func TestParseRedisAddrRejectsInvalidConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		addr    string
		message string
	}{
		{"empty", "", "is empty"},
		{"whitespace only", "   ", "is empty"},
		{"unsupported scheme", "https://cache.eu.internal:6379", `unsupported scheme "https"`},
		{"a different cache's scheme", "memcached://cache.eu.internal:11211", `unsupported scheme "memcached"`},
		{"no port", "cache.eu.internal", "host:port"},
		{"no host", ":6379", "no host"},
		{"port out of range", "cache.eu.internal:70000", "invalid port"},
		{"port zero", "cache.eu.internal:0", "invalid port"},
		{"non-numeric port", "cache.eu.internal:https", "invalid port"},
		{"unbracketed IPv6 is ambiguous", "rediss://::1:6379", "host:port"},
		{
			name:    "credentials are refused rather than silently dropped",
			addr:    "rediss://user:hunter2@cache.eu.internal:6379",
			message: "no path, query or credentials",
		},
		{
			name:    "a database number is refused rather than silently dropped",
			addr:    "rediss://cache.eu.internal:6379/0",
			message: "no path, query or credentials",
		},
		{
			name:    "a query string is refused rather than silently dropped",
			addr:    "rediss://cache.eu.internal:6379?db=1",
			message: "no path, query or credentials",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRedisAddr(tc.addr)
			require.Error(t, err, "%q must be rejected, not accepted and left to fail at connect time", tc.addr)
			require.Contains(t, err.Error(), tc.message)
		})
	}
}

// TestParseRedisAddrErrorsDoNotLeakCredentials asserts a rejected address does
// not echo a secret back into a log line or an operator's terminal.
func TestParseRedisAddrErrorsDoNotLeakCredentials(t *testing.T) {
	_, err := ParseRedisAddr("rediss://austro:hunter2@cache.eu.internal:6379")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "hunter2", "a configuration error must not print the password it was given")
}
