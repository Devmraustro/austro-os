package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// ExternalIDHasher hashes raw external identifiers irreversibly so a plain
// platform/reference value is never persisted or emitted (P8/P15 minimal
// disclosure, ADR-011). It is deterministic and replay-safe for a given HMAC
// key.
type ExternalIDHasher struct {
	key []byte
}

// NewExternalIDHasher builds a hasher bound to the supplied HMAC key. Empty
// keys yield a zero-value hasher that still returns deterministic digests (use
// the configured signing key in production wiring).
func NewExternalIDHasher(key []byte) *ExternalIDHasher {
	return &ExternalIDHasher{key: key}
}

// Hash returns the hex-encoded HMAC-SHA256 digest of raw. An empty raw value
// returns ErrEmptyValue. The output is a fixed, opaque token never reversible
// to the input.
func (h *ExternalIDHasher) Hash(raw string) (string, error) {
	if raw == "" {
		return "", ErrEmptyValue
	}
	mac := hmac.New(sha256.New, h.key)
	_, _ = mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil)), nil
}
