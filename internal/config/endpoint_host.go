package config

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// allowLoopbackEnv opts out of the loopback endpoint rule.
//
// The rule exists because a development address left in a deployment is
// indistinguishable from a working one: the process starts, dials itself, and
// the deployment passes until the day the address is reachable from somewhere
// else. It is not a claim that loopback is always wrong. CI runs its databases
// as containers with their ports published on the host and legitimately dials
// 127.0.0.1, so the opt-in exists for that case - but it has to be stated,
// rather than arrived at by writing an address the check happens to miss.
const allowLoopbackEnv = "AUSTRO_ALLOW_LOOPBACK"

// loopbackAllowed reports whether AUSTRO_ALLOW_LOOPBACK was set to an
// affirmative value.
//
// Only an exact affirmative spelling enables it. A typo must leave the rule in
// force: a typo that quietly disabled the check would restore the very
// false negative this replaces, which is the failure mode an opt-out is most
// likely to acquire.
func loopbackAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(allowLoopbackEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// isLoopbackEndpoint reports whether a configured endpoint is written as this
// machine, in the ways it can be written:
//
//	127.0.0.1:6379                a literal address
//	[::1]:6379                    the IPv6 literal, bracketed
//	0.0.0.0:6379                  the unspecified address, which as a
//	                              destination means this machine
//	localhost:6379                the name
//
// It applies to endpoint settings only. isInsecure covers the placeholder
// question for every setting, including secrets, and a secret is not a place
// where "localhost" is evidence of anything.
//
// It deliberately does not resolve names. The previous check compared the
// whole value against the literal strings "localhost:5432", "localhost:6379"
// and "localhost:5672", so 127.0.0.1:6379 - the form in every example file and
// every CI workflow - passed; that false negative is what this replaces, and
// literals cover it without a resolver.
//
// The cost of asking the resolver would be paid at every startup, and it is not
// worth it here: a hostname that resolves to a loopback address is a far
// narrower case than a loopback address written out, and a validator whose
// answer depends on a network service is a validator that can be stalled or
// flipped by one. That a name resolves elsewhere is a runtime observation, and
// the connection failure names it. So the limitation is stated rather than
// hidden: "localhost" and loopback literals are detected; a name pointing at
// this host is not.
func isLoopbackEndpoint(v string) bool {
	host := endpointHost(v)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isLocalIP(ip)
	}
	return false
}

// isLocalIP reports whether an address refers to this machine.
//
// 0.0.0.0 and :: are included deliberately. As a destination they mean this
// machine on every platform that accepts them, so a configuration naming them
// is not reaching a service elsewhere either.
func isLocalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsUnspecified()
}

// endpointHost extracts the host from a configured endpoint, which may be a URL
// carrying credentials, a path and a query, or a bare host:port.
//
// It returns "" when no host can be read, so an unparsable value is left to the
// rules that already report it rather than being guessed at here.
func endpointHost(v string) string {
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil {
			return ""
		}
		return u.Hostname()
	}
	if host, _, err := net.SplitHostPort(v); err == nil {
		return host
	}
	return ""
}
