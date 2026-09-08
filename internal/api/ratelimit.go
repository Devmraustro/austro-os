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

// windowEntry tracks the count for one fixed window.
type windowEntry struct {
	start time.Time
	count int
}

// RateLimiter is a concurrency-safe fixed-window limiter keyed by arbitrary
// strings (route + client address).
type RateLimiter struct {
	mu      sync.Mutex
	windows map[string]*windowEntry
}

// NewRateLimiter returns an empty per-process rate limiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{windows: map[string]*windowEntry{}}
}

// Allow reports whether the key may proceed under the rule. Windows are
// lazily reset, so memory grows only with distinct keys in a window.
func (l *RateLimiter) Allow(key string, rule rateRule) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= rule.window {
		l.windows[key] = &windowEntry{start: now, count: 1}
		return true
	}
	if w.count >= rule.max {
		return false
	}
	w.count++
	return true
}
