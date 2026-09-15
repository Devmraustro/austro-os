package audit

import (
	"austro-os/internal/config"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AuditEvent struct {
	ID         uuid.UUID `json:"id"`
	EventID    uuid.UUID `json:"event_id"`
	Timestamp  time.Time `json:"timestamp"`
	TraceID    uuid.UUID `json:"trace_id,omitempty"`
	SpanID     uuid.UUID `json:"span_id,omitempty"`
	ActorType  string    `json:"actor_type"`
	ActorID    uuid.UUID `json:"actor_id"`
	TargetType string    `json:"target_type"`
	TargetID   uuid.UUID `json:"target_id"`
	EventType  string    `json:"event_type"`
	Outcome    string    `json:"outcome"`
	// Hash chain
	HashParent      uuid.UUID `json:"hash_parent,omitempty"`
	HashValue       []byte    `json:"hash_value,omitempty"`
	HashGeneratedAt time.Time `json:"hash_generated_at,omitempty"`
	// Digital signature
	DigitalSignature     []byte    `json:"digital_signature,omitempty"`
	SignatureGeneratedAt time.Time `json:"signature_generated_at,omitempty"`
	// Genesis flag
	Genesis bool `json:"genesis,omitempty"`
	// Constitutional principle reference
	ConstitutionalPrinciple string `json:"constitutional_principle"`
}

const GENESIS_HASH = "0000000000000000000000000000000000000000000000000000000000000000"

// auditTimeFormat is the timestamp encoding bound into every hash: RFC3339 with
// nanoseconds, normalised to UTC, so the same event always hashes to the same
// bytes regardless of the zone the verifying process happens to run in.
const auditTimeFormat = time.RFC3339Nano

// GenesisEvent builds the cryptographic root of the append-only audit chain.
func GenesisEvent(principal string) *AuditEvent {
	now := time.Now().UTC()
	ev := &AuditEvent{
		ID:                      uuid.New(),
		EventID:                 uuid.New(),
		Timestamp:               now,
		HashParent:              uuid.Nil,
		HashGeneratedAt:         now,
		Genesis:                 true,
		ConstitutionalPrinciple: principal,
		Outcome:                 "genesis",
	}

	// The genesis root binds the chain constant together with the whole root
	// payload, not just the timestamp: a root that left the outcome or the
	// principle unbound could be rewritten without detection.
	hashBytes := sha256.Sum256([]byte(GenesisPreimage(ev)))
	ev.HashValue = hashBytes[:]

	// The genesis is the cryptographic root of the append-only audit chain and
	// must therefore be signed with the same HMAC-SHA256 authenticity tag used
	// for every subsequent event; an unsigned root would allow a forged chain.
	ev.DigitalSignature = signHash(ev.HashValue)
	ev.SignatureGeneratedAt = now
	return ev
}

// GenesisPreimage returns the canonical pre-image hashed to produce the genesis
// hash value. It is exported so a verifier recomputes exactly what was hashed
// instead of duplicating the format string and drifting from it.
func GenesisPreimage(ev *AuditEvent) string {
	var b strings.Builder
	writeField(&b, GENESIS_HASH)
	writeField(&b, ev.Timestamp.UTC().Format(auditTimeFormat))
	writeField(&b, ev.Outcome)
	writeField(&b, ev.ConstitutionalPrinciple)
	return b.String()
}

// ChainPreimage returns the canonical pre-image hashed to produce ev's chain
// value from its predecessor. Every field whose alteration must be detectable is
// bound here, including the outcome and the constitutional principle: leaving
// them out would let an attacker rewrite whether an audited operation succeeded
// without breaking the chain.
//
// Fields are length-prefixed rather than separated by a delimiter. A delimited
// encoding is ambiguous because the predecessor hash is binary and can contain
// the delimiter itself, which lets two different field tuples produce one
// pre-image; length-prefixing makes the encoding injective.
func ChainPreimage(prev, ev *AuditEvent) string {
	var b strings.Builder
	writeField(&b, prev.EventID.String())
	writeField(&b, hex.EncodeToString(prev.HashValue))
	writeField(&b, prev.Timestamp.UTC().Format(auditTimeFormat))
	writeField(&b, strconv.FormatBool(prev.Genesis))
	writeField(&b, ev.EventID.String())
	writeField(&b, ev.Timestamp.UTC().Format(auditTimeFormat))
	writeField(&b, ev.EventType)
	writeField(&b, ev.ActorType)
	writeField(&b, ev.ActorID.String())
	writeField(&b, ev.TargetType)
	writeField(&b, ev.TargetID.String())
	writeField(&b, ev.Outcome)
	writeField(&b, ev.ConstitutionalPrinciple)
	return b.String()
}

// writeField appends one length-prefixed field to a pre-image.
func writeField(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
	b.WriteByte(',')
}

// AppendEvent links a new event onto the chain rooted at prev.
func AppendEvent(prev *AuditEvent, eventType string, actorType string, actorID uuid.UUID, targetType string, targetID uuid.UUID, outcome string, principal string) *AuditEvent {
	now := time.Now().UTC()

	var parentUUID uuid.UUID
	if prev.ID != uuid.Nil {
		parentUUID = prev.ID
	}

	ev := &AuditEvent{
		ID:                      uuid.New(),
		EventID:                 uuid.New(),
		Timestamp:               now,
		HashParent:              parentUUID,
		HashGeneratedAt:         now,
		Genesis:                 false,
		ConstitutionalPrinciple: principal,
		Outcome:                 outcome,
		EventType:               eventType,
		ActorType:               actorType,
		ActorID:                 actorID,
		TargetType:              targetType,
		TargetID:                targetID,
	}

	hashBytes := sha256.Sum256([]byte(ChainPreimage(prev, ev)))
	ev.HashValue = hashBytes[:]
	ev.DigitalSignature = signHash(ev.HashValue)
	ev.SignatureGeneratedAt = now
	return ev
}

// signHash produces the HMAC-SHA256 authenticity tag for a chain hash value.
// The chain hash itself is an unkeyed SHA-256, so anyone able to write to the
// audit table could recompute it after an edit; the keyed tag is what makes an
// edit detectable by a party that does not hold the key.
func signHash(hashValue []byte) []byte {
	mac := hmac.New(sha256.New, []byte(config.Get().JWTSecret))
	mac.Write(hashValue)
	return mac.Sum(nil)
}

// VerifyHashChain reports whether a chain is intact and authentic. It is
// fail-closed: an empty chain is rejected rather than vacuously accepted,
// because a log with no genesis root carries no integrity guarantee at all.
// Every event must satisfy BOTH the unkeyed chain link and the keyed
// authenticity tag.
func VerifyHashChain(events []*AuditEvent) bool {
	if len(events) == 0 {
		return false
	}

	for i, event := range events {
		if event == nil {
			return false
		}
		if i == 0 {
			// Genesis handling: the first event is verified against the chain
			// constant, never against events[i-1].
			if !event.Genesis {
				return false
			}
			expected := sha256.Sum256([]byte(GenesisPreimage(event)))
			if !hmac.Equal(event.HashValue, expected[:]) {
				return false
			}
		} else {
			expected := sha256.Sum256([]byte(ChainPreimage(events[i-1], event)))
			if !hmac.Equal(event.HashValue, expected[:]) {
				return false
			}
		}
		// The keyed tag is checked as well as the chain link. Without this the
		// chain is only as strong as an unkeyed hash that an attacker with
		// write access to the table can simply recompute.
		if !hmac.Equal(event.DigitalSignature, signHash(event.HashValue)) {
			return false
		}
	}

	return true
}
