// Package health answers the one question a load balancer, a platform
// supervisor, or an operator at 3am actually has: is this deployment doing the
// whole job, or only the part that happens to live in this process?
//
// It exists because splitting the API, the dispatcher, and the worker across
// three processes creates a failure that no single process can see from the
// inside. An API that delegates the dispatch sweep to a companion on another
// host will start, serve traffic, and pass every check it can run on itself
// while no dispatcher exists anywhere. The evidence that closes that gap is
// the heartbeat table in infrastructure/postgres, and this package is the
// consumer: it turns "a companion was declared" and "a companion is beating"
// into two separate facts, so a deployment that is missing its dispatcher is
// reported as missing rather than assumed healthy.
package health

import (
	"context"
	"time"
)

// LivenessReader reads the observed liveness of one process role. It is an
// interface rather than a concrete store so the readiness decision can be
// tested against a scripted database, including the failure modes that matter
// most — a reader that errors, and a reader that lies.
type LivenessReader interface {
	// Liveness returns the current state of the named role. An error means the
	// state is unknown, which is not the same as the role being absent, and the
	// caller must not treat it as healthy.
	Liveness(ctx context.Context, role string, staleAfter time.Duration) (RoleStatus, error)
}

// RoleStatus is one role's observed liveness.
type RoleStatus struct {
	// Role is the process role these facts describe.
	Role string
	// Instances is how many instances of the role beat within the staleness
	// window. Zero means the role is not running, or is running somewhere that
	// cannot reach PostgreSQL; the two are indistinguishable from here and mean
	// the same thing to a caller.
	Instances int
	// LastSeen is when the most recent beat landed, zero when there never was
	// one. It is reported so an operator can tell "never started" from "stopped
	// 10 minutes ago" without reading logs.
	LastSeen time.Time
}

// Report is the result of one readiness evaluation: whether the deployment is
// serving, and the facts behind that answer.
type Report struct {
	// Ready is the verdict. It is false when any required dependency is
	// missing, and its companion Reason says which.
	Ready bool
	// Reason is a short, stable, machine-comparable code for the first
	// unsatisfied dependency. It is empty when Ready.
	Reason string
	// Required lists the roles this deployment declared it needs elsewhere. A
	// deployment that declares nothing reports an empty slice, which is how
	// "no companion required" is distinguished from "companion required and
	// missing".
	Required []string
	// Companions reports the observed state of every declared companion,
	// including the ones that are healthy. Reporting only the failures would
	// make a green deployment indistinguishable from one nobody checked.
	Companions []RoleStatus
	// CheckedAt is when the evaluation ran, for the same reason as LastSeen.
	CheckedAt time.Time
}

// Dependency describes what this process needs from processes it did not start.
type Dependency struct {
	// Role is the companion role, as named by AUSTRO_PROCESS_ROLE on the
	// companion.
	Role string
	// Required gates readiness when true. An optional dependency is reported
	// but does not fail the check, so a caller can observe a role it does not
	// need without coupling its own availability to that role.
	//
	// The entrypoint does not currently build optional entries. Observing a
	// role at all costs a database round trip, and a role nobody declared is
	// one a single-process deployment has no reason to pay for: when that query
	// failed, readiness would report 503 for a companion that was never
	// required. The capability stays because a declared-but-advisory role is a
	// real need, and the test coverage for it is what makes adding one later a
	// decision rather than a discovery.
	Required bool
}

// Evaluator computes readiness from declared dependencies and observed
// liveness.
//
// The two inputs are kept separate on purpose. The declaration is a fact about
// the deployment's shape, known at configuration time and entirely within this
// process's control. The liveness is a fact about other hosts, known only from
// the shared datastore. Conflating them would let a process either hide a
// missing companion (trust the declaration) or invent one (trust its own
// absence as permission to fail).
type Evaluator struct {
	reader       LivenessReader
	dependencies []Dependency
	staleAfter   time.Duration
	// now is injectable so the decision can be tested without a clock.
	now func() time.Time
}

// NewEvaluator returns an evaluator over reader for the given declared
// dependencies. staleAfter is how long a heartbeat remains evidence of a
// running process; it is passed per read rather than closed over so the same
// value the configuration validated is the one used.
func NewEvaluator(reader LivenessReader, deps []Dependency, staleAfter time.Duration) *Evaluator {
	return &Evaluator{
		reader:       reader,
		dependencies: deps,
		staleAfter:   staleAfter,
		now:          time.Now,
	}
}

// Evaluate produces the readiness report.
//
// An error reading liveness is a failed readiness check, not a degraded-but-ok
// one. The failure mode this guards is specific: a process whose database
// connection is broken cannot see its dispatcher, so reporting "no dispatcher
// found" and "could not look" as the same answer would let a network partition
// between the API and PostgreSQL look like a healthy deployment that happens to
// have no dispatcher.
func (e *Evaluator) Evaluate(ctx context.Context) (Report, error) {
	report := Report{Ready: true, CheckedAt: e.now()}

	for _, dep := range e.dependencies {
		if dep.Required {
			report.Required = append(report.Required, dep.Role)
		}
		status, err := e.reader.Liveness(ctx, dep.Role, e.staleAfter)
		if err != nil {
			// The role is reported with zero instances so the response still
			// names the dependency that could not be checked, and the error is
			// returned so the caller can log the cause rather than inventing
			// one.
			report.Companions = append(report.Companions, RoleStatus{Role: dep.Role})
			report.Ready = false
			report.Reason = "liveness-unavailable"
			return report, err
		}
		if status.Role == "" {
			status.Role = dep.Role
		}
		report.Companions = append(report.Companions, status)
		if dep.Required && status.Instances == 0 {
			report.Ready = false
			report.Reason = "companion-absent"
		}
	}

	return report, nil
}
