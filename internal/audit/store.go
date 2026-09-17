package audit

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// Record is one audited operation, described by the caller and completed by the
// sink. Callers supply the semantic content (what happened, to whom, with what
// outcome); the sink supplies the chain linkage, the signature and the
// timestamp, none of which a caller could be trusted to state correctly.
type Record struct {
	EventType  string
	ActorType  string
	ActorID    uuid.UUID
	TargetType string
	TargetID   uuid.UUID

	// Outcome is the security-relevant result: "success", "denied", "failed",
	// "replay_detected". It is bound into the chain hash, so rewriting a
	// failure into a success breaks the chain.
	Outcome string

	// Principle is the constitutional principle the operation is recorded
	// against. Also bound into the chain hash.
	Principle string

	// WorkspaceID scopes the record to a tenant. It is nil for
	// organization-level events (bootstrap, login, refresh replay), which have
	// no single workspace and are deliberately readable by every
	// workspace-scoped reader.
	WorkspaceID *uuid.UUID

	// TraceID/SpanID carry request correlation through to the audit record.
	TraceID uuid.UUID
	SpanID  uuid.UUID

	// Details are free-form, non-secret context. They pass through
	// RedactDetails before anything is persisted.
	Details map[string]any
}

// Sink persists audit records. The production implementation is
// infrastructure/auditstore, which writes to PostgreSQL and maintains the hash
// chain; tests may substitute another implementation, but only the PostgreSQL
// one is evidence that audit records survive a restart.
type Sink interface {
	Append(ctx context.Context, rec Record) (*AuditEvent, error)
}

// secretKeys are the substrings that make a detail key unsafe to persist. The
// match is a case-insensitive substring test on the key, deliberately broad:
// the cost of dropping a diagnostic field is a slightly less informative audit
// row, while the cost of persisting a credential is a compromised tenant.
var secretKeys = []string{
	"password", "passwd", "pwd",
	"secret", "token", "jwt", "credential",
	"authorization", "auth_header", "cookie", "session",
	"api_key", "apikey", "access_key", "private_key",
	"refresh", "signature", "hash",
}

// RedactDetails returns a copy of details with every key that looks like it
// could carry a credential removed. Values are never inspected and never
// copied into the error path, so a secret that reaches this function is
// dropped rather than logged on the way out.
func RedactDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	out := make(map[string]any, len(details))
	for k, v := range details {
		lower := strings.ToLower(k)
		redacted := false
		for _, bad := range secretKeys {
			if strings.Contains(lower, bad) {
				redacted = true
				break
			}
		}
		if redacted {
			out[k] = "[redacted]"
			continue
		}
		out[k] = v
	}
	return out
}

// NopSink discards records. It exists so a component that requires a sink can
// be constructed in a context with no persistence configured; it is never
// wired into a production path, and using one silently disables durable audit
// for the operations it covers.
type NopSink struct{}

// Append satisfies Sink without recording anything.
func (NopSink) Append(_ context.Context, _ Record) (*AuditEvent, error) { return nil, nil }
