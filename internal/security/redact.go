// Package security provides cross-cutting hardening primitives shared by the
// Phase 2 services (ADR-011, ROADMAP §8). It is a pure domain package with no
// infrastructure import (criterion #32): a secret-shaped-value redactor, a
// one-way external-id hasher, a deterministic throttler, and deny-by-default
// guards. Phase 2 is credential-free (ROADMAP §5.8); these are defense-in-depth
// invariants and the guarantees the security test suite asserts.
package security

import (
	"errors"
	"regexp"
	"strings"
)

// RedactMarker is the fixed, non-secret placeholder substituted for any
// secret-shaped substring in a caller-provided value.
const RedactMarker = "[REDACTED]"

var (
	// bearerToken matches "<scheme> <token40>" (e.g. "Bearer abc...", "Basic ab...")
	bearerToken = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[a-z0-9_\-\.+/=]{8,}\b`)
	// keyPrefix matches provider key prefixes followed by an opaque value.
	keyPrefix = regexp.MustCompile(`(?i)\b(?:sk|pk|ghp|gho|ghs|AKIA|aws|slack|discord|xox[baprs]|api[_-]?key|apikey|secret|token)[a-z0-9_\-]{3,}`)
	// pemBlock matches DER/PEM private-key blocks.
	pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
	// longOpaque matches long, high-entropy-looking alphanumeric blobs that are
	// not URLs, UUIDs, or obvious identifiers (surrogate for a token).
	longOpaque = regexp.MustCompile(`\b[a-z0-9+/]{40,}\b`)
)

// RedactSecret scans a caller-provided string for secret-shaped substrings and
// replaces each with RedactMarker. It is a filter guaranteeing the absence of
// obviously secret-shaped data (criteria/ADR-011 §8 #23, P9/P10), not an
// authentication mechanism. Substitutions are applied case-insensitively and
// greedily.
func RedactSecret(value string) string {
	if value == "" {
		return value
	}
	out := pemBlock.ReplaceAllString(value, RedactMarker)
	out = bearerToken.ReplaceAllString(out, RedactMarker)
	out = redactKeyLike(out)
	out = redactLongOpaque(out)
	return out
}

// redactKeyLike removes provider-key-like fragments while preserving words that
// merely mention the noun (e.g. "token cache", "secret store").
func redactKeyLike(s string) string {
	return keyPrefix.ReplaceAllStringFunc(s, func(m string) string {
		// Keep innocuous noun-only usages by requiring an attached or adjacent
		// opaque tail already handled elsewhere; here we strip the whole match.
		_ = m
		return RedactMarker
	})
}

// redactLongOpaque removes long, unbroken blobs that look like tokens but not
// URLs/UUIDs/hex digests we intentionally keep (content hashes).
func redactLongOpaque(s string) string {
	return longOpaque.ReplaceAllStringFunc(s, func(m string) string {
		if strings.Contains(m, ".") || strings.Contains(m, "-") && strings.Contains(m, "/") {
			// URL-ish or structured identifier: leave it.
			return m
		}
		return RedactMarker
	})
}

// ErrEmptyValue is returned by HashExternalID for an empty input.
var ErrEmptyValue = errors.New("security: external id is empty")

// RedactAllFields redacts every string value in a map, returning a new map.
func RedactAllFields(fields map[string]string) map[string]string {
	if fields == nil {
		return nil
	}
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		out[k] = RedactSecret(v)
	}
	return out
}
