package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"austro-os/internal/audit"

	"github.com/stretchr/testify/require"
)

// parseQuery runs parseAuditQuery against a query string and returns the
// resulting query, whether it was accepted, and the status the handler wrote.
func parseQuery(t *testing.T, raw string) (audit.Query, bool, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/audit/events?"+raw, nil)
	w := httptest.NewRecorder()
	q, ok := parseAuditQuery(w, req)
	return q, ok, w.Code
}

// TestParseAuditQueryRejectsMalformedInput pins the classification of bad input.
//
// Every one of these is a client mistake, so every one must be a 400. The
// over-long filter is the case that matters most: the reader enforces the same
// bound, but it reports it as an ordinary error, and an ordinary error from a
// read is served as a 500. Left to the reader, a 200-character event_type came
// back as an internal server error -- a server fault reported for client input.
func TestParseAuditQueryRejectsMalformedInput(t *testing.T) {
	for name, raw := range map[string]string{
		"over-long event_type": "event_type=" + strings.Repeat("x", 200),
		"over-long outcome":    "outcome=" + strings.Repeat("x", 200),
		"over-long actor_type": "actor_type=" + strings.Repeat("x", 200),
		// One past the documented bound, to show the limit is exact rather than
		// approximate.
		"event_type just over the bound": "event_type=" + strings.Repeat("x", 65),
		"non-numeric limit":              "limit=abc",
		"negative limit":                 "limit=-1",
		"non-numeric cursor":             "before_seq=notanumber",
		"negative cursor":                "before_seq=-5",
		// An unrecognized parameter is refused rather than ignored, so a
		// misspelled filter cannot silently return an unfiltered page.
		"unknown parameter":    "workspace_id=11111111-1111-1111-1111-111111111111",
		"misspelled filter":    "eventtype=auth.login",
		"empty parameter name": "=value",
	} {
		t.Run(name, func(t *testing.T) {
			_, ok, code := parseQuery(t, raw)
			require.False(t, ok, "%s must be refused", raw)
			require.Equal(t, http.StatusBadRequest, code,
				"%s is client input and must be a 400, not a server fault", raw)
		})
	}
}

// TestParseAuditQueryAppliesBounds covers the values that are accepted, and that
// an oversized limit is reduced rather than refused: a client probing the
// ceiling should get a page, not a different error path to compare against.
func TestParseAuditQueryAppliesBounds(t *testing.T) {
	t.Run("filter at the bound is accepted", func(t *testing.T) {
		q, ok, code := parseQuery(t, "event_type="+strings.Repeat("x", 64))
		require.True(t, ok)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, q.EventType, 64)
	})

	t.Run("oversized limit is clamped", func(t *testing.T) {
		q, ok, _ := parseQuery(t, "limit=100000")
		require.True(t, ok)
		require.Equal(t, audit.MaxPageSize, q.Limit,
			"the limit must be reduced to the documented maximum")
	})

	t.Run("absent limit falls back to the default", func(t *testing.T) {
		q, ok, _ := parseQuery(t, "outcome=success")
		require.True(t, ok)
		require.Equal(t, audit.DefaultPageSize, q.Limit)
		require.Equal(t, "success", q.Outcome)
	})

	t.Run("zero limit falls back to the default", func(t *testing.T) {
		q, ok, _ := parseQuery(t, "limit=0")
		require.True(t, ok)
		require.Equal(t, audit.DefaultPageSize, q.Limit)
	})

	t.Run("filters and cursor are carried through", func(t *testing.T) {
		q, ok, _ := parseQuery(t,
			"event_type=auth.login&outcome=denied&actor_type=user&before_seq=42&limit=7")
		require.True(t, ok)
		require.Equal(t, "auth.login", q.EventType)
		require.Equal(t, "denied", q.Outcome)
		require.Equal(t, "user", q.ActorType)
		require.EqualValues(t, 42, q.BeforeSeq)
		require.Equal(t, 7, q.Limit)
		require.Nil(t, q.Workspace,
			"the workspace is never taken from the query string; it comes from verified claims")
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		q, ok, _ := parseQuery(t, "event_type=%20auth.login%20")
		require.True(t, ok)
		require.Equal(t, "auth.login", q.EventType)
	})
}
