package austro_os_test

import (
	"testing"

	"austro-os/internal/event"
	"austro-os/internal/principlemapping"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestPrincipleEnforcement verifies the semantic event-type -> principle map:
// each event type is assigned exactly its required principle and nothing else.
func TestPrincipleEnforcement(t *testing.T) {
	pm := principlemapping.NewPrincipleMapper()

	expected := map[event.EventType]string{
		event.EventTypeCreated:            "Vision First",
		event.EventTypeUpdated:            "Quality Over Speed",
		event.EventTypeDeleted:            "Backward Compatibility",
		event.EventTypeAssigned:           "Human Oversight",
		event.EventTypeCompleted:          "Continuous Improvement",
		event.EventTypeFailed:             "Observability",
		event.EventTypeStarted:            "Quality Over Speed",
		event.EventTypeEscalated:          "Human Oversight",
		event.EventTypePrincipleViolation: "Security by Design",
	}

	for et, want := range expected {
		require.Equal(t, want, pm.Map(et), "event type %s must map to %s", et, want)
	}

	// Every registered event type has an enforceable, non-empty principle.
	for _, et := range principlemapping.AllEventTypes() {
		require.NotEmpty(t, pm.Map(event.EventType(et)), "every event type must have a principle")
	}
}

// engine returns a fully-populated, valid envelope ready for validation.
func principleEnvelope(eventType event.EventType, principle string) *event.UniversalEnvelope {
	env := event.NewEnvelope()
	env.EventID = uuid.New()
	env.EventType = eventType
	env.ActorType = "system"
	env.ActorID = uuid.New()
	env.TargetType = "workspace"
	env.TargetID = uuid.New()
	env.WorkspaceID = workspaceA
	env.Outcome = "success"
	env.ConstitutionalPrinciple = principle
	return env
}

func TestPrincipleValidationRejectsMissing(t *testing.T) {
	pm := principlemapping.NewPrincipleMapper()
	env := principleEnvelope(event.EventTypeCreated, "")
	require.Error(t, pm.ValidatePrinciple(env), "missing principle must fail")
	require.Error(t, env.ValidatePrinciple(pm.Map(event.EventTypeCreated)), "missing principle must fail at envelope level")
}

func TestPrincipleValidationRejectsWrong(t *testing.T) {
	pm := principlemapping.NewPrincipleMapper()
	// "created" requires "Vision First"; giving it a different valid principle
	// must be rejected.
	env := principleEnvelope(event.EventTypeCreated, "Security by Design")
	require.Error(t, pm.ValidatePrinciple(env), "wrong principle must fail")
}

func TestPrincipleValidationRejectsInvalid(t *testing.T) {
	pm := principlemapping.NewPrincipleMapper()
	env := principleEnvelope(event.EventTypeCreated, "Not A Real Principle")
	require.Error(t, pm.ValidatePrinciple(env), "invalid principle must fail")
}

func TestClientInjectedPrincipleRejected(t *testing.T) {
	// The principle must come from the system mapping, not be trusted from
	// arbitrary client input. If a client supplies a principle that contradicts
	// the event-type contract, it must be rejected even when non-empty.
	pm := principlemapping.NewPrincipleMapper()
	env := principleEnvelope(event.EventTypeUpdated, "Vision First") // "updated" requires Quality Over Speed
	require.Error(t, pm.ValidatePrinciple(env), "a client-injected contradictory principle must fail")

	// Correct system-supplied principle passes.
	ok := principleEnvelope(event.EventTypeUpdated, pm.Map(event.EventTypeUpdated))
	require.NoError(t, pm.ValidatePrinciple(ok))
}

// TestPrincipleCoverage100 verifies that every mapped principle is one of the
// 18 constitutional principles (no invented/unknown principles).
func TestPrincipleCoverage100(t *testing.T) {
	pm := principlemapping.NewPrincipleMapper()
	constitutional := map[string]bool{
		"Vision First":                         true,
		"Architecture Before Implementation":   true,
		"Documentation Is Part of the Product": true,
		"Quality Over Speed":                   true,
		"Modular Design":                       true,
		"Replaceability":                       true,
		"Separation of Concerns":               true,
		"AI Independence":                      true,
		"Security by Design":                   true,
		"Privacy by Design":                    true,
		"Human Oversight":                      true,
		"Continuous Improvement":               true,
		"Observability":                        true,
		"Backward Compatibility":               true,
		"Simplicity":                           true,
		"Scalability":                          true,
		"Explicit Decisions":                   true,
		"Founder Authority":                    true,
	}

	for _, et := range principlemapping.AllEventTypes() {
		p := pm.Map(event.EventType(et))
		require.True(t, constitutional[p], "mapped principle %q for %s must be a real constitutional principle", p, et)
	}
}
