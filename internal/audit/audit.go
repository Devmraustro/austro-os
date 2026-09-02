package audit

import (
	"austro-os/internal/config"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type AuditEvent struct {
	ID        uuid.UUID `json:"id"`
	EventID   uuid.UUID `json:"event_id"`
	Timestamp time.Time `json:"timestamp"`
	TraceID   uuid.UUID `json:"trace_id,omitempty"`
	SpanID    uuid.UUID `json:"span_id,omitempty"`
	ActorType string    `json:"actor_type"`
	ActorID   uuid.UUID `json:"actor_id"`
	TargetType string    `json:"target_type"`
	TargetID  uuid.UUID `json:"target_id"`
	EventType string    `json:"event_type"`
	Outcome   string    `json:"outcome"`
	// Hash chain
	HashParent     uuid.UUID `json:"hash_parent,omitempty"`
	HashValue      []byte  `json:"hash_value,omitempty"`
	HashGeneratedAt time.Time `json:"hash_generated_at,omitempty"`
	// Digital signature
	DigitalSignature []byte `json:"digital_signature,omitempty"`
	SignatureGeneratedAt time.Time `json:"signature_generated_at,omitempty"`
	// Genesis flag
	Genesis bool `json:"genesis,omitempty"`
	// Constitutional principle reference
	ConstitutionalPrinciple string `json:"constitutional_principle"`
}

const GENESIS_HASH = "0000000000000000000000000000000000000000000000000000000000000000"

func GenesisEvent(principal string) *AuditEvent {
	now := time.Now().UTC()
	hashBytes := sha256.Sum256([]byte(GENESIS_HASH + "." + now.Format("2006-01-02T15:04:05Z07:00")))

	// The genesis is the cryptographic root of the append-only audit chain and
	// must therefore be signed with the same HMAC-SHA256 authenticity tag used
	// for every subsequent event; an unsigned root would allow a forged chain.
	digitalSig := hmac.New(sha256.New, []byte(config.Get().JWTSecret))
	digitalSig.Write(hashBytes[:])
	sig := digitalSig.Sum(nil)

	return &AuditEvent{
		ID:                  uuid.New(),
		EventID:             uuid.New(),
		Timestamp:           now,
		HashParent:          uuid.Nil,
		HashValue:           hashBytes[:],
		HashGeneratedAt:     now,
		DigitalSignature:    sig,
		SignatureGeneratedAt: now,
		Genesis:             true,
		ConstitutionalPrinciple: principal,
		Outcome:             "genesis",
	}
}

func AppendEvent(prev *AuditEvent, eventType string, actorType string, actorID uuid.UUID, targetType string, targetID uuid.UUID, outcome string, principal string) *AuditEvent {
	now := time.Now().UTC()
	
	var parentUUID uuid.UUID
	if prev.ID != uuid.Nil {
		parentUUID = prev.ID
	}
	
	// Compute hash chain
	hashInput := fmt.Sprintf("%s|%s|%s|%v|%s|%v|%s|%s|%s",
		prev.HashValue,
		prev.EventID,
		prev.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
		prev.Genesis,
		eventType,
		actorType,
		actorID,
		targetType,
		targetID,
	)
	
	hashBytes := sha256.Sum256([]byte(hashInput))
	
	digitalSig := hmac.New(sha256.New, []byte(config.Get().JWTSecret))
	digitalSig.Write(hashBytes[:])
	sig := digitalSig.Sum(nil)
	
	return &AuditEvent{
		ID:                  uuid.New(),
		EventID:             uuid.New(),
		Timestamp:           now,
		HashParent:          parentUUID,
		HashValue:           hashBytes[:],
		DigitalSignature:    sig,
		SignatureGeneratedAt: now,
		Genesis:             false,
		ConstitutionalPrinciple: principal,
		Outcome:             outcome,
		EventType:           eventType,
		ActorType:           actorType,
		ActorID:             actorID,
		TargetType:          targetType,
		TargetID:            targetID,
	}
}

func VerifyHashChain(events []*AuditEvent) bool {
	if len(events) == 0 {
		return true
	}
	
	for i, event := range events {
		// Genesis handling: i==0 case
		if i == 0 {
			if !event.Genesis {
				return false
			}
			// Verify genesis hash
			expectedHash := sha256.Sum256([]byte(GENESIS_HASH + "." + event.Timestamp.Format("2006-01-02T15:04:05Z07:00")))
			if !hmac.Equal(event.HashValue[:], expectedHash[:]) {
				return false
			}
			continue
		}
		
		// Verify hash chain link
		prev := events[i-1]
		hashInput := fmt.Sprintf("%s|%s|%s|%v|%s|%v|%s|%s|%s",
			prev.HashValue,
			prev.EventID,
			prev.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
			prev.Genesis,
			event.EventType,
			event.ActorType,
			event.ActorID,
			event.TargetType,
			event.TargetID,
		)
		
		hashBytes := sha256.Sum256([]byte(hashInput))
		if !hmac.Equal(event.HashValue[:], hashBytes[:]) {
			return false
		}
	}
	
	return true
}