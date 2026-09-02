package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"austro-os/internal/event"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/google/uuid"
)

// CorrelationIDKey is the key for correlation ID in the context
type CorrelationIDKey struct{}

// TraceIDKey is the key for trace ID in the context
type TraceIDKey struct{}

// SpanIDKey is the key for span ID in the context
type SpanIDKey struct{}

// RequestIDKey is the key for request ID in the context
type RequestIDKey struct{}

// ContextValues holds correlation IDs from the HTTP request context
type ContextValues struct {
	RequestID string
	TraceID   string
	SpanID    string
	CorrelationID string
}

// ExtractContextValues extracts correlation IDs from an HTTP request.
// Returns empty strings if headers are not present.
func ExtractContextValues(r *http.Request) ContextValues {
	cv := ContextValues{
		RequestID: r.Header.Get("X-Request-ID"),
		TraceID:   r.Header.Get("X-Trace-ID"),
		SpanID:    r.Header.Get("X-Span-ID"),
		CorrelationID: r.Header.Get("X-Correlation-ID"),
	}

	// If no correlation ID, generate one from request ID
	if cv.CorrelationID == "" && cv.RequestID != "" {
		cv.CorrelationID = cv.RequestID
	}

	// If no trace ID, generate one
	if cv.TraceID == "" {
		cv.TraceID = generateTraceID()
	}

	// If no span ID, generate one
	if cv.SpanID == "" {
		cv.SpanID = generateSpanID()
	}

	return cv
}

// PropagateContextToEvent propagates correlation IDs from context to an event envelope.
// The event producer must obtain the principle from the system mapping, NOT from client input.
func PropagateContextToEvent(r *http.Request, ev *event.UniversalEnvelope) {
	cv := ExtractContextValues(r)
	
	if cv.TraceID != "" {
		ev.TraceID = uuid.MustParse(cv.TraceID)
	}
	if cv.SpanID != "" {
		ev.SpanID = uuid.MustParse(cv.SpanID)
	}
	
	if cv.CorrelationID != "" {
		ev.Details = jsonRawMessage(map[string]string{
			"correlation_id": cv.CorrelationID,
		})
	}
}

// PropagateContextToRabbitMQ propagates correlation IDs from context to RabbitMQ message headers.
func PropagateContextToRabbitMQ(r *http.Request) amqp.Table {
	cv := ExtractContextValues(r)
	headers := amqp.Table{
		"trace_id":      cv.TraceID,
		"span_id":       cv.SpanID,
		"correlation_id": cv.CorrelationID,
		"request_id":    cv.RequestID,
	}
	return headers
}

// jsonRawMessage creates a json.RawMessage from a string
func jsonRawMessage(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Middleware is an HTTP middleware that sets correlation IDs on the request context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract or generate correlation IDs
		cv := ExtractContextValues(r)
		
		// If no request ID, generate one
		if cv.RequestID == "" {
			cv.RequestID = generateRequestID()
			// Set the request ID header for downstream
			w.Header().Set("X-Request-ID", cv.RequestID)
		}
		
		// Set the trace ID and span ID headers
		w.Header().Set("X-Trace-ID", cv.TraceID)
		w.Header().Set("X-Span-ID", cv.SpanID)
		if cv.CorrelationID != "" {
			w.Header().Set("X-Correlation-ID", cv.CorrelationID)
		}
		
		// Continue to the next handler
		next.ServeHTTP(w, r)
	})
}

// VerifyHeaders checks that the required correlation headers are present.
// Returns an error if any required header is missing.
func VerifyHeaders(r *http.Request) error {
	cv := ExtractContextValues(r)
	
	if cv.RequestID == "" {
		return fmt.Errorf("missing required X-Request-ID header")
	}
	if cv.TraceID == "" {
		return fmt.Errorf("missing required X-Trace-ID header")
	}
	
	return nil
}

// generateRequestID generates a unique request ID
func generateRequestID() string {
	// In production, use a CSPRNG
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

// generateTraceID generates a unique trace ID
func generateTraceID() string {
	// In production, use a CSPRNG
	return fmt.Sprintf("trace-%d", time.Now().UnixNano())
}

// generateSpanID generates a unique span ID
func generateSpanID() string {
	// In production, use a CSPRNG
	return fmt.Sprintf("span-%d", time.Now().UnixNano())
}