package security

import (
	"sync"
	"time"
)

// bucket tracks the hits consumed within the current window for a single key.
type bucket struct {
	count  int
	opened time.Time
}

// Throttler is a fixed-window, per-key rate limiter shared by Phase 2
// abuse-control paths (ADR-011 §9, ROADMAP §5.9). It fails closed: once a key
// exhausts its window budget, Allow returns false until the window elapses.
// The window is honored (a key's budget resets when its window expires) and the
// backing map is bounded: once it grows past pruneThreshold, stale keys whose
// windows have expired are evicted so the limiter cannot be used as a memory
// leak under high key cardinality.
type Throttler struct {
	mu        sync.Mutex
	window    time.Duration
	limit     int
	buckets   map[string]bucket
	lastPrune time.Time
}

// pruneThreshold bounds the number of tracked keys; the map is pruned only when
// it is at least this large, and at most once per minute, to keep the common
// path cheap.
const pruneThreshold = 4096

// NewThrottler returns a limiter permitting up to limit hits per key per
// window. Invalid limits window to a safe default of 1 (deny-most).
func NewThrottler(window time.Duration, limit int) *Throttler {
	if window <= 0 {
		window = time.Minute
	}
	if limit <= 0 {
		limit = 1
	}
	return &Throttler{window: window, limit: limit, buckets: map[string]bucket{}}
}

// Allow reports whether key may proceed now, consuming one hit if so. It is
// deterministic for a given sequence of calls.
func (t *Throttler) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	b, ok := t.buckets[key]
	if !ok || now.Sub(b.opened) >= t.window {
		b = bucket{opened: now}
	}
	if b.count >= t.limit {
		t.buckets[key] = b
		return false
	}
	b.count++
	t.buckets[key] = b
	if len(t.buckets) >= pruneThreshold && now.Sub(t.lastPrune) > time.Minute {
		t.pruneLocked(now)
	}
	return true
}

// pruneLocked evicts keys whose window has fully elapsed. Caller holds the
// mutex.
func (t *Throttler) pruneLocked(now time.Time) {
	for k, b := range t.buckets {
		if now.Sub(b.opened) >= t.window {
			delete(t.buckets, k)
		}
	}
	t.lastPrune = now
}

// Reset clears the budget for key (used by tests and window resets).
func (t *Throttler) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.buckets, key)
}
