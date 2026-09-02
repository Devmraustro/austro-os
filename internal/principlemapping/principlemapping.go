package principlemapping

import (
	"fmt"

	"austro-os/internal/event"
)

type PrincipleMapper struct{}

func NewPrincipleMapper() *PrincipleMapper {
	return &PrincipleMapper{}
}

func (pm *PrincipleMapper) Map(eventType event.EventType) string {
	switch eventType {
	case event.EventTypeCreated:
		return "Vision First"
	case event.EventTypeUpdated:
		return "Quality Over Speed"
	case event.EventTypeDeleted:
		return "Backward Compatibility"
	case event.EventTypeAssigned:
		return "Human Oversight"
	case event.EventTypeCompleted:
		return "Continuous Improvement"
	case event.EventTypeFailed:
		return "Observability"
	case event.EventTypeStarted:
		return "Quality Over Speed"
	case event.EventTypeEscalated:
		return "Human Oversight"
	case event.EventTypePrincipleViolation:
		return "Security by Design"
	default:
		return ""
	}
}

// ConstitutionalPrinciples is the authoritative registry of the 18 principles
// documented in CONSTITUTION.md. It is the complete set the platform enforces.
var ConstitutionalPrinciples = []string{
	"Vision First",
	"Architecture Before Implementation",
	"Documentation Is Part of the Product",
	"Quality Over Speed",
	"Modular Design",
	"Replaceability",
	"Separation of Concerns",
	"AI Independence",
	"Security by Design",
	"Privacy by Design",
	"Human Oversight",
	"Continuous Improvement",
	"Observability",
	"Backward Compatibility",
	"Simplicity",
	"Scalability",
	"Explicit Decisions",
	"Founder Authority",
}

// AllPrinciples returns the full registry of 18 constitutional principles.
func AllPrinciples() []string {
	return ConstitutionalPrinciples
}

func (pm *PrincipleMapper) RequiredPrinciples() map[string]string {
	m := make(map[string]string)
	eventTypes := []event.EventType{
		event.EventTypeCreated,
		event.EventTypeUpdated,
		event.EventTypeDeleted,
		event.EventTypeAssigned,
		event.EventTypeCompleted,
		event.EventTypeFailed,
		event.EventTypeStarted,
		event.EventTypeEscalated,
		event.EventTypePrincipleViolation,
	}
	for _, et := range eventTypes {
		principle := pm.Map(et)
		if principle != "" {
			m[eventPrincipleKey(et)] = principle
		}
	}
	// The full registry of 18 principles is part of what the platform requires.
	for _, p := range ConstitutionalPrinciples {
		m[principleRegistryKey(p)] = p
	}
	return m
}

func eventPrincipleKey(et event.EventType) string {
	return "event:" + string(et)
}

func principleRegistryKey(p string) string {
	return "principle:" + p
}

func (pm *PrincipleMapper) ValidatePrinciple(ev *event.UniversalEnvelope) error {
	required := pm.Map(ev.EventType)
	if required == "" {
		return nil
	}

	if ev.ConstitutionalPrinciple == "" {
		return fmt.Errorf("missing constitutional principle in event %s", ev.EventType)
	}

	if ev.ConstitutionalPrinciple != required {
		return fmt.Errorf("event %s requires principle %s but got %s",
			ev.EventType, required, ev.ConstitutionalPrinciple)
	}

	return nil
}

func AllEventTypes() []string {
	return []string{
		string(event.EventTypeCreated),
		string(event.EventTypeUpdated),
		string(event.EventTypeDeleted),
		string(event.EventTypeAssigned),
		string(event.EventTypeCompleted),
		string(event.EventTypeFailed),
		string(event.EventTypeStarted),
		string(event.EventTypeEscalated),
		string(event.EventTypePrincipleViolation),
	}
}
