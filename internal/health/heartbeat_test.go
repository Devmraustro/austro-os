package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeReader is a scripted LivenessReader. It is deliberately not a mock with
// recorded expectations: these tests are about what the evaluator concludes from
// a set of facts, and the interesting facts are the ones a real database makes
// awkward to arrange — a role that never ran, a role that ran and stopped, and a
// database that cannot be reached at all.
type fakeReader struct {
	statuses map[string]RoleStatus
	err      error
	calls    []string
}

func (f *fakeReader) Liveness(_ context.Context, role string, _ time.Duration) (RoleStatus, error) {
	f.calls = append(f.calls, role)
	if f.err != nil {
		return RoleStatus{Role: role}, f.err
	}
	status, ok := f.statuses[role]
	if !ok {
		// A role with no row is a role that has never run, not an error.
		return RoleStatus{Role: role}, nil
	}
	return status, nil
}

var testNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// withClock pins the evaluator's clock so the report's timestamp is assertable
// without depending on when the test happens to run.
func withClock(e *Evaluator) *Evaluator {
	e.now = func() time.Time { return testNow }
	return e
}

// TestNoDeclaredCompanionsMeansNoRequirement is the compatibility case. A
// single-process deployment declares nothing, so readiness is unconditional and
// the historical behaviour is untouched — no database round trip, no 503, no
// new reason code for a deployment that has no companions to be missing.
func TestNoDeclaredCompanionsMeansNoRequirement(t *testing.T) {
	reader := &fakeReader{statuses: map[string]RoleStatus{}}
	e := withClock(NewEvaluator(reader, nil, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.NoError(t, err)
	require.True(t, report.Ready)
	require.Empty(t, report.Required)
	require.Empty(t, report.Companions)
	require.Empty(t, report.Reason)
	require.Equal(t, testNow, report.CheckedAt)
	require.Empty(t, reader.calls, "with nothing declared, nothing should be queried")
}

// TestDeclaredCompanionThatIsRunningIsReady is the healthy split deployment.
func TestDeclaredCompanionThatIsRunningIsReady(t *testing.T) {
	reader := &fakeReader{statuses: map[string]RoleStatus{
		"dispatcher": {Role: "dispatcher", Instances: 1, LastSeen: testNow.Add(-5 * time.Second)},
	}}
	e := withClock(NewEvaluator(reader, []Dependency{{Role: "dispatcher", Required: true}}, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.NoError(t, err)
	require.True(t, report.Ready, "a declared dispatcher that is beating is a ready deployment")
	require.Empty(t, report.Reason)
	require.Equal(t, []string{"dispatcher"}, report.Required)
	require.Len(t, report.Companions, 1)
	require.Equal(t, 1, report.Companions[0].Instances)
}

// TestDeclaredCompanionThatIsAbsentFailsReadiness is the whole point of the
// mechanism. The declaration is present and the process starts cleanly, so
// nothing about configuration reveals the gap; only the observation does.
func TestDeclaredCompanionThatIsAbsentFailsReadiness(t *testing.T) {
	reader := &fakeReader{statuses: map[string]RoleStatus{}}
	e := withClock(NewEvaluator(reader, []Dependency{{Role: "dispatcher", Required: true}}, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.NoError(t, err, "an absent companion is an answer, not a failure to answer")
	require.False(t, report.Ready)
	require.Equal(t, "companion-absent", report.Reason)
	require.Equal(t, []string{"dispatcher"}, report.Required,
		"the response must name what is missing, not merely that something is")
	require.Equal(t, RoleStatus{Role: "dispatcher"}, report.Companions[0],
		"the missing role is still reported, so the failure is attributable")
}

// TestUnreadableLivenessIsNotTheSameAsAbsentCompanion is the distinction that
// keeps this honest under a network partition. If a failed read and an empty
// result collapsed to the same answer, then a broken connection between the API
// and PostgreSQL would look exactly like a healthy deployment that happens to
// have no dispatcher — the precise confusion the table exists to remove.
func TestUnreadableLivenessIsNotTheSameAsAbsentCompanion(t *testing.T) {
	boom := errors.New("connection refused")
	reader := &fakeReader{err: boom}
	e := withClock(NewEvaluator(reader, []Dependency{{Role: "dispatcher", Required: true}}, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.Error(t, err, "the caller must be able to log why the check could not run")
	require.ErrorIs(t, err, boom)
	require.False(t, report.Ready)
	require.Equal(t, "liveness-unavailable", report.Reason,
		"'I could not look' and 'I looked and it is gone' are different failures")
	require.NotEqual(t, "companion-absent", report.Reason)
	require.Len(t, report.Companions, 1)
	require.Equal(t, "dispatcher", report.Companions[0].Role,
		"even an unreadable check must name the dependency it could not verify")
}

// TestOneLiveInstanceIsEnough pins the threshold. The contract is that the sweep
// happens, not that it happens N times, so a deployment deliberately scaled to a
// single dispatcher must not report itself unhealthy.
func TestOneLiveInstanceIsEnough(t *testing.T) {
	reader := &fakeReader{statuses: map[string]RoleStatus{
		"dispatcher": {Role: "dispatcher", Instances: 1},
	}}
	e := withClock(NewEvaluator(reader, []Dependency{{Role: "dispatcher", Required: true}}, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.NoError(t, err)
	require.True(t, report.Ready)
}

// TestOptionalCompanionIsReportedButDoesNotGate is the difference between "this
// deployment needs a worker" and "this deployment has one". An API that admitted
// work before the first worker was deployed is a real state, and taking it out
// of rotation over a queue that was never going to have a consumer would be a
// worse failure than the one being guarded.
func TestOptionalCompanionIsReportedButDoesNotGate(t *testing.T) {
	reader := &fakeReader{statuses: map[string]RoleStatus{}}
	e := withClock(NewEvaluator(reader, []Dependency{{Role: "worker", Required: false}}, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.NoError(t, err)
	require.True(t, report.Ready, "an undeclared optional companion must not fail readiness")
	require.Empty(t, report.Required)
	require.Len(t, report.Companions, 1, "it is still reported, so the state is visible")
	require.Equal(t, 0, report.Companions[0].Instances)
}

// TestOneMissingRequiredCompanionFailsEvenWhenOthersAreHealthy checks that a
// single satisfied dependency cannot mask an unsatisfied one. The loop must not
// stop at the first success, and a deployment with a dispatcher but no worker
// must not pass on the dispatcher's account.
func TestOneMissingRequiredCompanionFailsEvenWhenOthersAreHealthy(t *testing.T) {
	reader := &fakeReader{statuses: map[string]RoleStatus{
		"dispatcher": {Role: "dispatcher", Instances: 1},
	}}
	e := withClock(NewEvaluator(reader, []Dependency{
		{Role: "dispatcher", Required: true},
		{Role: "worker", Required: true},
	}, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.NoError(t, err)
	require.False(t, report.Ready)
	require.Equal(t, "companion-absent", report.Reason)
	require.Equal(t, []string{"dispatcher", "worker"}, report.Required)
	require.Len(t, report.Companions, 2)
}

// TestHealthyCompanionsAreReportedToo guards the reporting contract in the other
// direction. Returning only the failures would make a green deployment
// indistinguishable from one nobody checked — the same ambiguity this mechanism
// was added to remove, just moved.
func TestHealthyCompanionsAreReportedToo(t *testing.T) {
	reader := &fakeReader{statuses: map[string]RoleStatus{
		"dispatcher": {Role: "dispatcher", Instances: 1, LastSeen: testNow},
		"worker":     {Role: "worker", Instances: 2, LastSeen: testNow},
	}}
	e := withClock(NewEvaluator(reader, []Dependency{
		{Role: "dispatcher", Required: true},
		{Role: "worker", Required: true},
	}, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.NoError(t, err)
	require.True(t, report.Ready)
	require.Len(t, report.Companions, 2)
	for _, c := range report.Companions {
		require.NotZero(t, c.LastSeen,
			"a healthy companion must carry evidence of when it last reported")
	}
}

// TestReaderReturningAnUnlabelledStatusIsAttributedToTheDependency keeps the
// report self-consistent. A store that omits the role would otherwise produce a
// companion entry with an empty name, which is impossible to act on.
func TestReaderReturningAnUnlabelledStatusIsAttributedToTheDependency(t *testing.T) {
	reader := &fakeReader{statuses: map[string]RoleStatus{
		"dispatcher": {Instances: 1},
	}}
	e := withClock(NewEvaluator(reader, []Dependency{{Role: "dispatcher", Required: true}}, time.Minute))

	report, err := e.Evaluate(context.Background())
	require.NoError(t, err)
	require.Equal(t, "dispatcher", report.Companions[0].Role)
}

// TestStalenessWindowIsPassedThrough confirms the evaluator does not apply its
// own notion of staleness. The value the configuration validated must be the one
// used, or a deployment could pass a strict setting and be evaluated against a
// looser one.
func TestStalenessWindowIsPassedThrough(t *testing.T) {
	var got time.Duration
	reader := &staleCapturingReader{onCall: func(d time.Duration) { got = d }}
	e := NewEvaluator(reader, []Dependency{{Role: "dispatcher", Required: true}}, 90*time.Second)

	_, err := e.Evaluate(context.Background())
	require.NoError(t, err)
	require.Equal(t, 90*time.Second, got)
}

type staleCapturingReader struct {
	onCall func(time.Duration)
}

func (s *staleCapturingReader) Liveness(_ context.Context, role string, staleAfter time.Duration) (RoleStatus, error) {
	s.onCall(staleAfter)
	return RoleStatus{Role: role, Instances: 1}, nil
}
