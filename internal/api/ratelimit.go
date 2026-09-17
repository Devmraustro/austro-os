package api

import (
	"sync"
	"time"
)

// rateRule bounds a fixed window of requests: at most max in any window
// period. The limiter is deliberately in-memory and per-instance: it is a
// defense against casual abuse and credential stuffing, not a CDN/WAF
// replacement.
type rateRule struct {
	window time.Duration
	max    int
}

var (
	loginRateRule     = rateRule{window: time.Minute, max: 10}
	refreshRateRule   = rateRule{window: time.Minute, max: 20}
	logoutRateRule    = rateRule{window: time.Minute, max: 30}
	bootstrapRateRule = rateRule{window: time.Minute, max: 5}
)

// maxRateLimitKeys bounds the number of distinct keys the limiter tracks.
// Keys are route+client-address, so an attacker able to present many source
// addresses (a botnet, or a rotating proxy) would otherwise grow this map
// without limit: an entry was only ever reset when the same key came back,
// never reclaimed. 65536 keys is well beyond any realistic client population
// for these four routes while keeping the table in the low megabytes.
const maxRateLimitKeys = 1 << 16

// rateLimitSweepBatch is how many stale entries a sweep drops when the table is
// at its bound, so the sweep cost stays amortised O(1) per request instead of
// scanning the table on every call.
const rateLimitSweepBatch = 1 << 12

// rateLimitSweepThreshold sits one batch below the hard bound so a sweep always
// has room to bring the table back down before the bound is reached.
const rateLimitSweepThreshold = maxRateLimitKeys - rateLimitSweepBatch

// windowEntry tracks the count for one fixed window.
type windowEntry struct {
	start  time.Time
	count  int
	window time.Duration
}

// RateLimiter is a concurrency-safe fixed-window limiter keyed by arbitrary
// strings (route + client address).
type RateLimiter struct {
	mu      sync.Mutex
	windows map[string]*windowEntry
	// lastSweep is when the table was last reclaimed, and sweepInterval is the
	// minimum spacing between sweeps. Sweeping on a timer as well as at the
	// size bound keeps the table small when traffic is low, while the spacing
	// keeps the O(n) scan off the per-request path.
	lastSweep     time.Time
	sweepInterval time.Duration
}

// rateLimitSweepInterval is the default spacing between reclamation sweeps. It
// equals the window of every configured rule, so a key is reclaimed as soon as
// its own window can have elapsed.
const rateLimitSweepInterval = time.Minute

// NewRateLimiter returns an empty per-process rate limiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		windows:       map[string]*windowEntry{},
		lastSweep:     time.Now(),
		sweepInterval: rateLimitSweepInterval,
	}
}

// Allow reports whether the key may proceed under the rule.
func (l *RateLimiter) Allow(key string, rule rateRule) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()

	w, ok := l.windows[key]
	if !ok {
		// A new key is the only thing that can grow the table, so reclamation is
		// gated on it: an existing key just increments. A sweep runs either on
		// its interval or as soon as the table nears the bound.
		if now.Sub(l.lastSweep) >= l.sweepInterval || len(l.windows) >= rateLimitSweepThreshold {
			l.lastSweep = now
			l.sweepLocked(now)
		}
		l.windows[key] = &windowEntry{start: now, count: 1, window: rule.window}
		return true
	}
	if now.Sub(w.start) >= w.window {
		// Lazily reset an elapsed window in place; the key already exists so
		// the table does not grow.
		w.start, w.count, w.window = now, 1, rule.window
		return true
	}
	if w.count >= rule.max {
		return false
	}
	w.count++
	return true
}

// sweepLocked reclaims keys whose window has fully elapsed, and if that is not
// enough, drops the oldest keys outright. Caller holds the lock.
func (l *RateLimiter) sweepLocked(now time.Time) {
	for k, w := range l.windows {
		if now.Sub(w.start) >= w.window {
			delete(l.windows, k)
		}
	}
	if len(l.windows) < rateLimitSweepThreshold {
		return
	}
	// Every remaining key is inside a live window, which means the limiter is
	// genuinely under load from more distinct clients than the bound allows.
	// Dropping the oldest keys is the safe direction: it forgets a counter
	// rather than denying a request, and the affected client simply starts a
	// fresh window.
	type entry struct {
		key   string
		start time.Time
	}
	oldest := make([]entry, 0, rateLimitSweepBatch)
	for k, w := range l.windows {
		if len(oldest) < rateLimitSweepBatch {
			oldest = append(oldest, entry{k, w.start})
			continue
		}
		newest := 0
		for i := 1; i < len(oldest); i++ {
			if oldest[i].start.After(oldest[newest].start) {
				newest = i
			}
		}
		if w.start.Before(oldest[newest].start) {
			oldest[newest] = entry{k, w.start}
		}
	}
	for _, e := range oldest {
		delete(l.windows, e.key)
	}
}
