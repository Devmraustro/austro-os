package memory

import (
	"errors"
	"strings"
	"time"
)

// Sentinel errors reported by the workspace-scoped memory boundary.
var (
	ErrWorkspaceRequired = errors.New("memory: workspace id is required")
	ErrInvalidKey        = errors.New("memory: invalid key")
	ErrSecretRejected    = errors.New("memory: secret-shaped content is not stored")
	ErrValueTooLarge     = errors.New("memory: value exceeds the maximum size")
	ErrInvalidTTL        = errors.New("memory: invalid ttl")
	ErrRateLimited       = errors.New("memory: write rate limit exceeded")
	ErrNotFound          = errors.New("memory: not found")
)

// Bounds enforced at the facade boundary (abuse controls, ROADMAP §8).
const (
	maxKeyLength  = 256
	maxValueBytes = 64 * 1024
	maxTTL        = 90 * time.Hour // 90 days upper bound
)

// ValidLayer reports whether l is a recognized memory layer.
func ValidLayer(l MemoryLayer) bool {
	switch l {
	case LayerSession, LayerLongTerm, LayerWorkspace, LayerOrganizational:
		return true
	}
	return false
}

// layerString returns the partition segment for a layer.
func layerString(l MemoryLayer) string {
	switch l {
	case LayerSession:
		return "session"
	case LayerLongTerm:
		return "longterm"
	case LayerWorkspace:
		return "workspace"
	case LayerOrganizational:
		return "org"
	}
	return "unknown"
}

// validateKey rejects blank/oversized keys and keys that would collide with a
// partition or another namespace (partition-prefix confusion, P15 minimal
// disclosure). It returns ErrInvalidKey when the key is unusable.
func validateKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return ErrInvalidKey
	}
	if len(key) > maxKeyLength {
		return ErrInvalidKey
	}
	if strings.ContainsAny(key, "\x00\r\n") {
		return ErrInvalidKey
	}
	return nil
}
