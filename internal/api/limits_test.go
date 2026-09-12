package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRateLimiterStillEnforcesTheRule is the behavioural baseline: bounding the
// table must not change what the limiter permits.
func TestRateLimiterStillEnforcesTheRule(t *testing.T) {
	l := NewRateLimiter()
	rule := rateRule{window: time.Minute, max: 3}

	for i := 0; i < 3; i++ {
		if !l.Allow("login:10.0.0.1", rule) {
			t.Fatalf("request %d within the budget was denied", i+1)
		}
	}
	if l.Allow("login:10.0.0.1", rule) {
		t.Error("the request over budget must be denied")
	}
	// A different key is independent.
	if !l.Allow("login:10.0.0.2", rule) {
		t.Error("a different client must have its own budget")
	}
}

// TestRateLimiterReclaimsElapsedWindows is the regression test for the leak:
// the comment on Allow claimed windows were "lazily reset, so memory grows only
// with distinct keys in a window", but a key was only ever reset when the same
// key came back, so keys from clients that never returned accumulated forever.
func TestRateLimiterReclaimsElapsedWindows(t *testing.T) {
	l := NewRateLimiter()
	// Reclamation is spaced by sweepInterval so the O(n) scan stays off the
	// per-request path. Hold sweeps off while seeding so the key count is
	// deterministic, then let one run.
	l.sweepInterval = time.Hour
	rule := rateRule{window: 20 * time.Millisecond, max: 5}

	for i := 0; i < 500; i++ {
		l.Allow(fmt.Sprintf("login:192.0.2.%d", i), rule)
	}
	l.mu.Lock()
	before := len(l.windows)
	l.mu.Unlock()
	if before != 500 {
		t.Fatalf("expected 500 live keys, got %d", before)
	}

	time.Sleep(30 * time.Millisecond)

	// One new key must trigger the sweep that reclaims the elapsed ones.
	l.sweepInterval = 0
	l.Allow("login:203.0.113.1", rule)
	l.mu.Lock()
	after := len(l.windows)
	l.mu.Unlock()
	if after != 1 {
		t.Errorf("elapsed windows must be reclaimed, leaving only the new key; got %d", after)
	}
}

// TestRateLimiterIsBoundedUnderDistinctKeys proves the table cannot grow without
// limit even when every key is inside a live window, which is the case a sweep
// of expired entries alone cannot handle.
func TestRateLimiterIsBoundedUnderDistinctKeys(t *testing.T) {
	l := NewRateLimiter()
	rule := rateRule{window: time.Hour, max: 5}

	for i := 0; i < maxRateLimitKeys+1024; i++ {
		l.Allow(fmt.Sprintf("refresh:203.0.113.%d:%d", i/65536, i%65536), rule)
	}
	l.mu.Lock()
	size := len(l.windows)
	l.mu.Unlock()
	if size > maxRateLimitKeys {
		t.Errorf("limiter exceeded its bound: %d > %d", size, maxRateLimitKeys)
	}
}

// TestDecodeJSONRejectsTrailingData covers the strict-parsing requirement: a
// body carrying a second JSON value after the parsed object must be refused
// rather than silently truncated to the first value.
func TestDecodeJSONRejectsTrailingData(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"single object", `{"username":"founder","password":"pw"}`, true},
		{"object with surrounding whitespace", "  {\"username\":\"founder\",\"password\":\"pw\"}\n", true},
		{"second object appended", `{"username":"founder","password":"pw"}{"username":"attacker","password":"x"}`, false},
		{"trailing garbage", `{"username":"founder","password":"pw"}garbage`, false},
		{"unknown field", `{"username":"founder","password":"pw","role":"founder"}`, false},
		{"malformed", `{"username":`, false},
		{"empty body", ``, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(tc.body))
			var req credentialsRequest
			got := decodeJSON(w, r, &req)
			if got != tc.ok {
				t.Fatalf("decodeJSON accepted=%v, want %v (status %d)", got, tc.ok, w.Code)
			}
			if !tc.ok && w.Code != http.StatusBadRequest {
				t.Errorf("a rejected body must produce 400, got %d", w.Code)
			}
		})
	}
}

// TestDecodeJSONEnforcesBodyLimit proves the size bound is actually applied and
// that reading past it is refused rather than buffered.
func TestDecodeJSONEnforcesBodyLimit(t *testing.T) {
	w := httptest.NewRecorder()
	big := `{"username":"` + strings.Repeat("a", maxRequestBodyBytes) + `","password":"pw"}`
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(big))
	var req credentialsRequest
	if decodeJSON(w, r, &req) {
		t.Error("a body over the limit must be rejected")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for an oversized body, got %d", w.Code)
	}
}
