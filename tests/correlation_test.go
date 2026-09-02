package austro_os_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"austro-os/internal/event"
	"austro-os/internal/middleware"

	"github.com/stretchr/testify/require"
)

// TestExtractContextValues verifies correlation IDs are extracted from request
// headers and that missing trace/span IDs are auto-generated.
func TestExtractContextValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-1")
	req.Header.Set("X-Trace-ID", "trace-from-client")
	req.Header.Set("X-Span-ID", "span-from-client")
	req.Header.Set("X-Correlation-ID", "corr-from-client")

	cv := middleware.ExtractContextValues(req)
	require.Equal(t, "req-1", cv.RequestID)
	require.Equal(t, "trace-from-client", cv.TraceID)
	require.Equal(t, "span-from-client", cv.SpanID)
	require.Equal(t, "corr-from-client", cv.CorrelationID)

	// Correlation ID falls back to request ID when the former is absent.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("X-Request-ID", "req-2")
	cv2 := middleware.ExtractContextValues(req2)
	require.Equal(t, "req-2", cv2.CorrelationID)

	// Missing trace/span are auto-generated (non-empty).
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	cv3 := middleware.ExtractContextValues(req3)
	require.NotEmpty(t, cv3.TraceID)
	require.NotEmpty(t, cv3.SpanID)
}

// TestMiddlewareEchoesCorrelationHeaders verifies the middleware propagates the
// auto-generated correlation identifiers back on the HTTP response.
func TestMiddlewareEchoesCorrelationHeaders(t *testing.T) {
	// Request carries no correlation headers, so the middleware must generate
	// and echo trace/span/request ids on the response.
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	rec := httptest.NewRecorder()
	middleware.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)

	require.NotEmpty(t, rec.Header().Get("X-Request-ID"), "middleware must generate and echo X-Request-ID")
	require.NotEmpty(t, rec.Header().Get("X-Trace-ID"), "middleware must generate and echo X-Trace-ID")
	require.NotEmpty(t, rec.Header().Get("X-Span-ID"), "middleware must generate and echo X-Span-ID")
}

// TestPropagateContextToEvent verifies correlation IDs flow into the audit
// event envelope. Uses real UUID-valued headers so the propagation path is
// exercised end-to-end.
func TestPropagateContextToEvent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Trace-ID", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	req.Header.Set("X-Span-ID", "11111111-2222-3333-4444-555555555555")
	req.Header.Set("X-Correlation-ID", "corr-event")

	env := event.NewEnvelope()
	middleware.PropagateContextToEvent(req, env)

	require.Equal(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", env.TraceID.String())
	require.Equal(t, "11111111-2222-3333-4444-555555555555", env.SpanID.String())
	require.NotNil(t, env.Details, "correlation id must be embedded in the envelope details")
	require.Contains(t, string(env.Details), "corr-event")
}

// TestPropagateContextToRabbitMQ verifies correlation IDs are carried into
// RabbitMQ message headers for downstream workers.
func TestPropagateContextToRabbitMQ(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-r")
	req.Header.Set("X-Trace-ID", "trace-r")
	req.Header.Set("X-Span-ID", "span-r")
	req.Header.Set("X-Correlation-ID", "corr-r")

	headers := middleware.PropagateContextToRabbitMQ(req)
	require.Equal(t, "req-r", headers["request_id"])
	require.Equal(t, "trace-r", headers["trace_id"])
	require.Equal(t, "span-r", headers["span_id"])
	require.Equal(t, "corr-r", headers["correlation_id"])
}

// TestVerifyHeaders verifies the middleware refuses requests lacking the
// required correlation headers. Only X-Request-ID is enforced: trace/span ids
// are auto-generated when absent, so they can never fail this check.
func TestVerifyHeaders(t *testing.T) {
	okReq := httptest.NewRequest(http.MethodGet, "/", nil)
	okReq.Header.Set("X-Request-ID", "r")
	require.NoError(t, middleware.VerifyHeaders(okReq))

	noReq := httptest.NewRequest(http.MethodGet, "/", nil)
	require.Error(t, middleware.VerifyHeaders(noReq), "missing X-Request-ID must fail")
}

// TestContextCorrelationKeys verifies the exported context key types are
// usable for round-tripping correlation IDs through a context.
func TestContextCorrelationKeys(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, middleware.TraceIDKey{}, "trace-ctx")
	ctx = context.WithValue(ctx, middleware.SpanIDKey{}, "span-ctx")
	require.Equal(t, "trace-ctx", ctx.Value(middleware.TraceIDKey{}))
	require.Equal(t, "span-ctx", ctx.Value(middleware.SpanIDKey{}))
}
