package security

import (
	"sync"
	"time"
)

// Throttler is a fixed-window, per-key rate limiter shared by Phase 2
// abuse-control paths (ADR-011 §9, ROADMAP §5.9). It fails closed: once a key
// exhausts its window budget, Allow returns false until the window elapses.
type Throttler struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	buckets map[string]int
}

// NewThrottler returns a limiter permitting up to limit hits per key per
// window. Invalid limits window to a safe default of 1 (deny-most).
func NewThrottler(window time.Duration, limit int) *Throttler {
	if window <= 0 {
		window = time.Minute
	}
	if limit <= 0 {
		limit = 1
	}
	return &Throttler{window: window, limit: limit, buckets: map[string]int{}}
}

// Allow reports whether key may proceed now, consuming one hit if so. It is
// deterministic for a given sequence of calls.
func (t *Throttler) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buckets[key] >= t.limit {
		return false
	}
	t.buckets[key]++
	return true
}

// Reset clears the budget for key (used by tests and window resets).
func (t *Throttler) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.buckets, key)
}
