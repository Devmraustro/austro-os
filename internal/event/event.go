package event

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type EventType string

const (
	EventTypeCreated            EventType = "created"
	EventTypeUpdated            EventType = "updated"
	EventTypeDeleted            EventType = "deleted"
	EventTypeAssigned           EventType = "assigned"
	EventTypeCompleted          EventType = "completed"
	EventTypeFailed             EventType = "failed"
	EventTypeStarted            EventType = "started"
	EventTypeEscalated          EventType = "escalated"
	EventTypePrincipleViolation EventType = "principle_violation"
)

type UniversalEnvelope struct {
	EventID                 uuid.UUID       `json:"event_id"`
	Timestamp               time.Time       `json:"timestamp"`
	WorkspaceID             string          `json:"workspace_id,omitempty"`
	TraceID                 uuid.UUID       `json:"trace_id,omitempty"`
	SpanID                  uuid.UUID       `json:"span_id,omitempty"`
	ActorType               string          `json:"actor_type"`
	ActorID                 uuid.UUID       `json:"actor_id"`
	TargetType              string          `json:"target_type"`
	TargetID                uuid.UUID       `json:"target_id"`
	EventType               EventType       `json:"event_type"`
	Outcome                 string          `json:"outcome"`
	OutcomeDetails          json.RawMessage `json:"outcome_details,omitempty"`
	PermissionsChecked      map[string]bool `json:"permissions_checked,omitempty"`
	ConstitutionalPrinciple string          `json:"constitutional_principle"`
	Details                 json.RawMessage `json:"details,omitempty"`
	Genesis                 bool            `json:"genesis,omitempty"`
}

func NewEnvelope() *UniversalEnvelope {
	return &UniversalEnvelope{
		EventID:            uuid.New(),
		Timestamp:          time.Now().UTC(),
		OutcomeDetails:     nil,
		PermissionsChecked: make(map[string]bool),
	}
}

func (e *UniversalEnvelope) MarshalJSON() ([]byte, error) {
	type Alias UniversalEnvelope
	return json.Marshal(struct {
		*Alias
	}{
		Alias: (*Alias)(e),
	})
}

func (e *UniversalEnvelope) ValidatePrinciple(requiredPrinciple string) error {
	if e.ConstitutionalPrinciple == "" {
		return fmt.Errorf("missing constitutional principle in event %s", e.EventType)
	}
	if e.ConstitutionalPrinciple != requiredPrinciple {
		return fmt.Errorf("event %s requires principle %s but got %s", e.EventType, requiredPrinciple, e.ConstitutionalPrinciple)
	}
	return nil
}

type EventBus interface {
	Publish(*UniversalEnvelope) error
	Subscribe(string, func(*UniversalEnvelope) error)
	Broadcast(*UniversalEnvelope) error
}

type inProcessBus struct {
	subscribers map[string][]func(*UniversalEnvelope) error
}

func NewInProcessBus() *inProcessBus {
	return &inProcessBus{
		subscribers: make(map[string][]func(*UniversalEnvelope) error),
	}
}

func (bus *inProcessBus) Publish(ev *UniversalEnvelope) error {
	for _, handler := range bus.subscribers[string(ev.EventType)] {
		if err := handler(ev); err != nil {
			return err
		}
	}
	return nil
}

func (bus *inProcessBus) Subscribe(eventType string, handler func(*UniversalEnvelope) error) {
	bus.subscribers[eventType] = append(bus.subscribers[eventType], handler)
}

func (bus *inProcessBus) Broadcast(ev *UniversalEnvelope) error {
	return bus.Publish(ev)
}
