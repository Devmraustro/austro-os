package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Schemes accepted in AUSTRO_REDIS_ADDR.
const (
	// RedisScheme is plaintext, stated explicitly.
	RedisScheme = "redis"
	// RedisTLSScheme requires an encrypted, certificate-verified connection.
	RedisTLSScheme = "rediss"
)

// RedisEndpoint is a parsed AUSTRO_REDIS_ADDR.
type RedisEndpoint struct {
	// Addr is the host:port the client dials, with any scheme removed.
	Addr string
	// Host is Addr without its port and without IPv6 brackets. It is the name a
	// server certificate has to be issued to, and the value the certificate is
	// verified against.
	Host string
	// TLS reports whether the connection must be encrypted and verified.
	TLS bool
}

// redisAddrForms names the accepted forms. It is appended to every rejection so
// an operator who mistyped one is told what to write instead, rather than being
// left to infer it from whichever word happened to be wrong.
const redisAddrForms = "accepted forms: host:port, redis://host:port, rediss://host:port"

// redisAddrError reports a rejected AUSTRO_REDIS_ADDR. The message names the
// setting, the reason, and the accepted forms.
func redisAddrError(reason string) error {
	return fmt.Errorf("%s %s; %s", "AUSTRO_REDIS_ADDR", reason, redisAddrForms)
}

// ParseRedisAddr parses AUSTRO_REDIS_ADDR into the endpoint the client dials.
//
// Exactly three forms are accepted:
//
//	host:port          the legacy form, plaintext, behaviour unchanged
//	redis://host:port  plaintext, stated explicitly
//	rediss://host:port encrypted, with the server certificate verified
//
// The scheme is recognized only when followed by "://". A bare "redis:6379" is
// therefore the legacy form naming a host that happens to be called "redis" -
// the value used throughout the Compose files, the CI workflows and the test
// fixtures - and reading it as a scheme would parse the host as empty.
//
// A port is required. Without one the client cannot dial, so rejecting it here
// names the setting instead of surfacing an opaque network error at startup.
func ParseRedisAddr(addr string) (RedisEndpoint, error) {
	value := strings.TrimSpace(addr)
	if value == "" {
		return RedisEndpoint{}, redisAddrError("is empty")
	}

	scheme, hostPort := RedisScheme, value
	if i := strings.Index(value, "://"); i >= 0 {
		scheme, hostPort = strings.ToLower(value[:i]), value[i+len("://"):]
		if scheme != RedisScheme && scheme != RedisTLSScheme {
			return RedisEndpoint{}, redisAddrError(fmt.Sprintf("has unsupported scheme %q", scheme))
		}
	}

	// Credentials, a database number and a path have no meaning to Redis.
	// Accepting them would silently discard whatever the operator believed they
	// had configured, which is how a password ends up in an env file and a
	// connection ends up unauthenticated.
	if strings.ContainsAny(hostPort, "/?#@") {
		return RedisEndpoint{}, redisAddrError("must be host:port with no path, query or credentials")
	}

	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return RedisEndpoint{}, redisAddrError("must be host:port: " + err.Error())
	}
	if host == "" {
		return RedisEndpoint{}, redisAddrError("has no host")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return RedisEndpoint{}, redisAddrError(fmt.Sprintf("has invalid port %q", port))
	}

	return RedisEndpoint{Addr: hostPort, Host: host, TLS: scheme == RedisTLSScheme}, nil
}
