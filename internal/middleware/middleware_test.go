package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"austro-os/internal/event"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestGeneratedIDsAreValidUUIDs verifies the middleware's generated
// request/trace/span IDs are version-4 UUIDs. These strings are later parsed as
// uuid.UUID when propagated to event envelopes and RabbitMQ headers; a
// non-UUID value would panic the producer/consumer boundary (uuid.MustParse).
func TestGeneratedIDsAreValidUUIDs(t *testing.T) {
	for i := 0; i < 25; i++ {
		for _, id := range []string{
			generateRequestID(),
			generateTraceID(),
			generateSpanID(),
		} {
			parsed, err := uuid.Parse(id)
			require.NoError(t, err, "generated id %q must parse as a UUID", id)
			require.Equal(t, byte(4), byte(parsed.Version()), "generated id %q must be a v4 UUID", id)
		}
	}
}

// TestPropagateContextToEventNeverPanics verifies trace/span propagation is
// panic-free when the caller supplies no headers (generators must produce ids
// that uuid.MustParse accepts) and when the caller supplies invalid UUIDs
// (a hostile caller must not crash the producer).
func TestPropagateContextToEventNeverPanics(t *testing.T) {
	newReq := func(headers map[string]string) *http.Request {
		req := httptest.NewRequest("GET", "/health", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req
	}

	// No headers at all: generated ids must survive MustParse.
	ev := &event.UniversalEnvelope{}
	require.NotPanics(t, func() { PropagateContextToEvent(newReq(nil), ev) })
	require.NotNil(t, ev.TraceID)
	require.NotNil(t, ev.SpanID)

	// Hostile non-UUID headers: producer must not panic and must not propagate garbage.
	ev = &event.UniversalEnvelope{}
	require.NotPanics(t, func() {
		PropagateContextToEvent(newReq(map[string]string{
			"X-Trace-ID": "not-a-uuid",
			"X-Span-ID":  "../etc/passwd",
		}), ev)
	})
	require.Equal(t, uuid.Nil, ev.TraceID)
	require.Equal(t, uuid.Nil, ev.SpanID)
}

// TestPropagateContextToRabbitMQVerbatim verifies RabbitMQ headers carry the
// generated (UUID) trace/span ids verbatim, matching what the consumer parses.
func TestPropagateContextToRabbitMQVerbatim(t *testing.T) {
	req := httptest.NewRequest("GET", "/health", nil)
	headers := PropagateContextToRabbitMQ(req)
	for _, h := range []string{"trace_id", "span_id"} {
		v, ok := headers[h].(string)
		require.True(t, ok, "header %s must be a string", h)
		require.NoError(t, uuid.Validate(v), "RabbitMQ header %s must be a valid UUID, got %q", h, v)
	}
}
