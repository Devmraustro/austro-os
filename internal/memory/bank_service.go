package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	logger "austro-os/internal/log"
	"github.com/google/uuid"
)

// cellStore is the narrow persistence port the memory facade needs: only the
// four memory layers. It is satisfied by repository.Repository implementations
// (and by fakes in tests). Keeping memory against a narrow port instead of the
// whole repository preserves interface segregation (Separation of Concerns).
// ErrorAuditSink is an optional stronger audit contract. Existing in-memory
// sinks intentionally remain fire-and-forget, while the production adapter can
// fail closed when a persistent audit append fails.
type ErrorAuditSink interface {
	AuditSink
	RecordError(ctx context.Context, rec AuditRecord) error
}

type cellStore interface {
	StoreSessionMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error
	RetrieveSessionMemory(ctx context.Context, key string) ([]byte, error)
	DeleteSessionMemory(ctx context.Context, key string) error

	StoreLongTermMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error
	RetrieveLongTermMemory(ctx context.Context, key string) ([]byte, error)
	DeleteLongTermMemory(ctx context.Context, key string) error

	StoreWorkspaceMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error
	RetrieveWorkspaceMemory(ctx context.Context, key string) ([]byte, error)
	DeleteWorkspaceMemory(ctx context.Context, key string) error

	StoreOrganizationalMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error
	RetrieveOrganizationalMemory(ctx context.Context, key string) ([]byte, error)
	DeleteOrganizationalMemory(ctx context.Context, key string) error
}

// Bank is the workspace-scoped memory facade over the four memory layers. Unlike
// the Phase 1 layers, this facade requires an explicit WorkspaceID on every call
// (deny-by-default) instead of reading a process-global, so a workspace-B
// principal cannot derive or read workspace-A keys (Privacy by Design / minimal
// disclosure). It validates keys/values/TTLs, applies a per-workspace write
// throttle, refuses secret-shaped content, and emits audited, trace-correlated
// JSON logs.
type Bank struct {
	store    cellStore
	audit    AuditSink
	events   EventSink
	throttle *throttle
}

// NewBank wires a workspace-scoped memory facade. store is required; audit and
// events default to no-ops when nil.
func NewBank(store cellStore, audit AuditSink, events EventSink) *Bank {
	if audit == nil {
		audit = NullAuditSink{}
	}
	if events == nil {
		events = NullEventSink{}
	}
	return &Bank{store: store, audit: audit, events: events, throttle: newThrottle(100)}
}

// Write stores value under key in the given layer, scoped to workspaceID for
// session/long-term/workspace layers and org-wide for the organizational layer.
// It validates the key, bounds the value, caps the TTL, refuses secrets, and
// throttles per-workspace writes. Returns the effective TTL actually applied.
func (b *Bank) Write(ctx context.Context, workspaceID uuid.UUID, layer MemoryLayer, key string, value []byte, ttl time.Duration) error {
	if workspaceID == uuid.Nil {
		return ErrWorkspaceRequired
	}
	if !ValidLayer(layer) {
		return ErrInvalidLayer
	}
	if err := validateKey(key); err != nil {
		return err
	}
	if len(value) > maxValueBytes {
		return ErrValueTooLarge
	}
	if isSecretShaped(value) {
		return ErrSecretRejected
	}
	if ttl < 0 {
		return ErrInvalidTTL
	}
	if ttl > maxTTL {
		ttl = maxTTL
	}
	if !b.throttle.Allow(workspaceID) {
		return ErrRateLimited
	}

	cell := b.partitionKey(workspaceID, layer, key)
	var err error
	switch layer {
	case LayerSession:
		err = b.store.StoreSessionMemory(ctx, cell, value, ttl)
	case LayerLongTerm:
		err = b.store.StoreLongTermMemory(ctx, cell, value, ttl)
	case LayerWorkspace:
		err = b.store.StoreWorkspaceMemory(ctx, cell, value, ttl)
	case LayerOrganizational:
		err = b.store.StoreOrganizationalMemory(ctx, cell, value, ttl)
	}
	if err != nil {
		_ = b.recordAudit(ctx, AuditRecord{
			EventType: "memory.write", ActorType: actorTypeOf(ctx), ActorID: actorIDOf(ctx), ConstitutionalPrinciple: "Security by Design",
			Outcome: "failed", WorkspaceID: workspaceID.String(), Layer: layerString(layer), Key: key,
			TraceID: traceOf(ctx), SpanID: spanOf(ctx),
		})
		return err
	}
	if err := b.recordAudit(ctx, AuditRecord{
		EventType: "memory.write", ActorType: actorTypeOf(ctx), ActorID: actorIDOf(ctx), ConstitutionalPrinciple: "Privacy by Design",
		Outcome: "success", WorkspaceID: workspaceID.String(), Layer: layerString(layer), Key: key,
		TraceID: traceOf(ctx), SpanID: spanOf(ctx),
	}); err != nil {
		// A successful write without its durable audit record is not a
		// successful API mutation. Remove the cell best-effort before returning
		// the failure so the Redis store does not retain an unaudited orphan.
		_ = b.deleteCell(ctx, workspaceID, layer, cell)
		return err
	}
	_ = b.events.Publish(ctx, "memory.written", workspaceID, layerString(layer), key, traceOf(ctx), spanOf(ctx))
	log(workspaceID, "memory-written").With("layer", layerString(layer)).With("key", key).Log()
	return nil
}

// Read returns the value stored under key in the given layer within the
// supplied workspace. It is scoped so a workspace-B principal cannot read A.
func (b *Bank) Read(ctx context.Context, workspaceID uuid.UUID, layer MemoryLayer, key string) ([]byte, error) {
	if workspaceID == uuid.Nil {
		return nil, ErrWorkspaceRequired
	}
	if !ValidLayer(layer) {
		return nil, ErrInvalidLayer
	}
	if err := validateKey(key); err != nil {
		return nil, err
	}
	cell := b.partitionKey(workspaceID, layer, key)
	var out []byte
	var err error
	switch layer {
	case LayerSession:
		out, err = b.store.RetrieveSessionMemory(ctx, cell)
	case LayerLongTerm:
		out, err = b.store.RetrieveLongTermMemory(ctx, cell)
	case LayerWorkspace:
		out, err = b.store.RetrieveWorkspaceMemory(ctx, cell)
	case LayerOrganizational:
		out, err = b.store.RetrieveOrganizationalMemory(ctx, cell)
	}
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, ErrNotFound
	}
	if err := b.recordAudit(ctx, AuditRecord{
		EventType: "memory.read", ActorType: actorTypeOf(ctx), ActorID: actorIDOf(ctx), ConstitutionalPrinciple: "Minimal Disclosure",
		Outcome: "success", WorkspaceID: workspaceID.String(), Layer: layerString(layer), Key: key,
		TraceID: traceOf(ctx), SpanID: spanOf(ctx),
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes a cell within the supplied workspace.
func (b *Bank) Delete(ctx context.Context, workspaceID uuid.UUID, layer MemoryLayer, key string) error {
	if workspaceID == uuid.Nil {
		return ErrWorkspaceRequired
	}
	if !ValidLayer(layer) {
		return ErrInvalidLayer
	}
	if err := validateKey(key); err != nil {
		return err
	}
	cell := b.partitionKey(workspaceID, layer, key)
	var err error
	switch layer {
	case LayerSession:
		err = b.store.DeleteSessionMemory(ctx, cell)
	case LayerLongTerm:
		err = b.store.DeleteLongTermMemory(ctx, cell)
	case LayerWorkspace:
		err = b.store.DeleteWorkspaceMemory(ctx, cell)
	case LayerOrganizational:
		err = b.store.DeleteOrganizationalMemory(ctx, cell)
	}
	if err != nil {
		return err
	}
	if err := b.recordAudit(ctx, AuditRecord{
		EventType: "memory.delete", ActorType: actorTypeOf(ctx), ActorID: actorIDOf(ctx), ConstitutionalPrinciple: "Privacy by Design",
		Outcome: "success", WorkspaceID: workspaceID.String(), Layer: layerString(layer), Key: key,
	}); err != nil {
		return err
	}
	return nil
}

// partitionKey returns a namespace-isolated cell key for the given layer. The
// workspace-facing layers carry the workspace prefix; the organizational layer
// is org-wide and shared (never part of any one workspace's partition).
func (b *Bank) partitionKey(workspaceID uuid.UUID, layer MemoryLayer, key string) string {
	if layer == LayerOrganizational {
		return fmt.Sprintf("org:%s", key)
	}
	return fmt.Sprintf("workspace:%s:%s:%s", workspaceID.String(), layerString(layer), key)
}

// isSecretShaped reports whether a value looks like a credential/token. The
// concrete workforce never stores provider keys or tokens in memory (ADR-005,
// ROADMAP §5.8); this guard refuses the common shapes at the boundary.
func isSecretShaped(value []byte) bool {
	low := strings.ToLower(strings.TrimSpace(string(value)))
	if len(low) == 0 {
		return false
	}
	// Markers that are not ordinary English: a substring match anywhere is
	// enough.
	for _, marker := range []string{"-----begin", "bearer ", "api_key=", "apikey=", "secret="} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	// The short provider prefixes "sk-"/"pk-" are also substrings of ordinary
	// words ("task-list", "risk-free", "desk-drawer", "ask-follow"), so a bare
	// Contains rejects legitimate memory. A real key opens a token: it is at
	// the start of the value or follows a non-word character. Requiring that
	// boundary removes the false positives without losing detection of an
	// actual "sk-..."/"pk-..." key.
	for _, prefix := range []string{"sk-", "pk-"} {
		if hasTokenBoundary(low, prefix) {
			return true
		}
	}
	return false
}

// hasTokenBoundary reports whether prefix occurs at the start of s or directly
// after a non-word byte, i.e. at the start of a token rather than inside one.
func hasTokenBoundary(s, prefix string) bool {
	for from := 0; ; {
		idx := strings.Index(s[from:], prefix)
		if idx < 0 {
			return false
		}
		idx += from
		if idx == 0 || !isWordByte(s[idx-1]) {
			return true
		}
		from = idx + 1
	}
}

// isWordByte reports whether b can be part of a word token. s is already
// lower-cased by the caller, so only lower-case letters need to be considered.
func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// AuditSink records memory decisions for the append-only, principle-tagged
// chain. Implementations are provided at the composition boundary.
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord)
}

// AuditRecord is the memory service's audit contract. Only non-secret metadata.
type AuditRecord struct {
	EventType               string
	ActorType               string
	ActorID                 string
	ConstitutionalPrinciple string
	Outcome                 string
	WorkspaceID             string
	Layer                   string
	Key                     string
	TraceID                 string
	SpanID                  string
}

// recordAudit invokes the stronger production contract when available, while
// preserving compatibility with the existing fire-and-forget test sinks.
func (b *Bank) recordAudit(ctx context.Context, rec AuditRecord) error {
	if sink, ok := b.audit.(ErrorAuditSink); ok {
		return sink.RecordError(ctx, rec)
	}
	b.audit.Record(ctx, rec)
	return nil
}

func (b *Bank) deleteCell(ctx context.Context, workspaceID uuid.UUID, layer MemoryLayer, cell string) error {
	switch layer {
	case LayerSession:
		return b.store.DeleteSessionMemory(ctx, cell)
	case LayerLongTerm:
		return b.store.DeleteLongTermMemory(ctx, cell)
	case LayerWorkspace:
		return b.store.DeleteWorkspaceMemory(ctx, cell)
	case LayerOrganizational:
		return b.store.DeleteOrganizationalMemory(ctx, cell)
	default:
		return ErrInvalidLayer
	}
}

// NullAuditSink discards decisions.
type NullAuditSink struct{}

// Record implements AuditSink by discarding.
func (NullAuditSink) Record(_ context.Context, _ AuditRecord) {}

// EventSink publishes memory domain events.
type EventSink interface {
	Publish(ctx context.Context, eventType string, workspaceID uuid.UUID, layer, key, traceID, spanID string) error
}

// NullEventSink is a no-op publisher.
type NullEventSink struct{}

// Publish implements EventSink as a no-op.
func (NullEventSink) Publish(_ context.Context, _ string, _ uuid.UUID, _, _, _, _ string) error {
	return nil
}

// throttle is a simple fixed-window per-workspace write limiter.
type throttle struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	hits   map[uuid.UUID][]time.Time
}

func newThrottle(limit int) *throttle {
	if limit <= 0 {
		limit = 100
	}
	return &throttle{window: time.Second, limit: limit, hits: map[uuid.UUID][]time.Time{}}
}

func (t *throttle) Allow(workspaceID uuid.UUID) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	timestamps := t.hits[workspaceID]
	cutoff := now.Add(-t.window)
	kept := timestamps[:0]
	for _, ts := range timestamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= t.limit {
		t.hits[workspaceID] = kept
		return false
	}
	t.hits[workspaceID] = append(kept, now)
	return true
}

func log(workspaceID uuid.UUID, msg string) *logger.Entry {
	return logger.NewEntry(msg).With("workspace_id", workspaceID)
}

type traceKey struct{}
type spanKey struct{}
type actorKey struct{}

// WithActor attaches the verified actor to the context used by the HTTP
// adapter. The value is never accepted from a request body or header.
func WithActor(ctx context.Context, actorID string) context.Context {
	return context.WithValue(ctx, actorKey{}, actorID)
}

func actorTypeOf(ctx context.Context) string {
	if v := actorIDOf(ctx); v != "" {
		return "user"
	}
	return "system"
}

func actorIDOf(ctx context.Context) string {
	if v, ok := ctx.Value(actorKey{}).(string); ok {
		return v
	}
	return ""
}

// WithTrace attaches a trace/span id to ctx for audit propagation.
func WithTrace(ctx context.Context, trace, span string) context.Context {
	ctx = context.WithValue(ctx, traceKey{}, trace)
	ctx = context.WithValue(ctx, spanKey{}, span)
	return ctx
}

func traceOf(ctx context.Context) string {
	if v, ok := ctx.Value(traceKey{}).(string); ok {
		return v
	}
	return ""
}

func spanOf(ctx context.Context) string {
	if v, ok := ctx.Value(spanKey{}).(string); ok {
		return v
	}
	return ""
}
