package austro_os_test

// Live-infrastructure policy for the tests package.
//
// This package is the live integration suite: it exercises PostgreSQL,
// RabbitMQ, Redis and the running API as deployed, against compose hostnames.
// It therefore has one policy, applied through the helpers below rather than
// decided per test file.
//
//  1. Default: live infrastructure is REQUIRED. An unreachable service is a
//     failure, not a skip. A release gate that cannot reach its datastore has
//     verified nothing, and reporting that as green is the specific failure mode
//     this policy exists to prevent.
//  2. Explicit offline mode: AUSTRO_TEST_MODE=offline lets a
//     infrastructure-dependent test skip with a reason naming both the missing
//     dependency and the opt-in. The mode is never inferred from a failed
//     connection and never the default, so a typo or an absent variable cannot
//     quietly turn a gate into a suite of skips.
//  3. A gate additionally sets AUSTRO_REQUIRE_LIVE_TESTS=1. That contradicts
//     offline mode, and the contradiction is reported as a failure rather than
//     resolved in favour of skipping. The same variable also asserts that at
//     least one live dependency was actually reached during the run, so a gate
//     cannot pass having contacted nothing.
//
// Skips that are not about infrastructure availability -- a chain too short to
// page, a missing release artifact -- are unaffected: those tests are
// inapplicable, not unverified. TestNoInfrastructureSkipsOutsideThePolicy
// enforces that the two kinds are not confused.

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// testModeEnv selects how this package treats unavailable infrastructure.
	testModeEnv = "AUSTRO_TEST_MODE"

	// offlineTestMode is the only accepted value that permits an
	// infrastructure-dependent test to skip. Other values are rejected rather
	// than treated as offline, so "off", "0" and "true" cannot enable it.
	offlineTestMode = "offline"

	// requireLiveEnv is set by a release gate. It means "this run must exercise
	// live infrastructure", which offline mode cannot satisfy.
	requireLiveEnv = "AUSTRO_REQUIRE_LIVE_TESTS"
)

// livePolicy is the resolved policy for one run.
type livePolicy struct {
	// offline permits infrastructure-dependent tests to skip.
	offline bool
	// requireLive is set by a release gate and forbids offline mode.
	requireLive bool
}

// currentLivePolicy resolves the policy from the environment and rejects a
// contradiction outright. A gate that asks for offline mode is a
// misconfiguration, and failing here is the only outcome that cannot be read as
// a pass.
func currentLivePolicy(t *testing.T) livePolicy {
	t.Helper()
	p := livePolicy{
		offline:     strings.EqualFold(strings.TrimSpace(os.Getenv(testModeEnv)), offlineTestMode),
		requireLive: strings.TrimSpace(os.Getenv(requireLiveEnv)) == "1",
	}
	if p.offline && p.requireLive {
		t.Fatalf("%s=%s contradicts %s=1: a release gate must not run in offline mode, because skipping "+
			"the live tests is the one outcome the gate exists to detect. Unset %s, or run without %s.",
			testModeEnv, offlineTestMode, requireLiveEnv, testModeEnv, requireLiveEnv)
	}
	return p
}

// liveContactLedger records which dependencies a run actually reached, so a
// release gate can assert it exercised something rather than merely completing.
var (
	liveContactMu sync.Mutex
	liveContacts  = map[string]int{}
)

func recordLiveContact(dependency string) {
	liveContactMu.Lock()
	defer liveContactMu.Unlock()
	liveContacts[dependency]++
}

func liveContactSnapshot() map[string]int {
	liveContactMu.Lock()
	defer liveContactMu.Unlock()
	out := make(map[string]int, len(liveContacts))
	for k, v := range liveContacts {
		out[k] = v
	}
	return out
}

// requiredLiveContact lists the dependencies a gate must have reached. Both are
// load-bearing: without PostgreSQL there is no persistence to assert, and
// without the API there is no request path to assert.
var requiredLiveContact = []string{"PostgreSQL", "API"}

// enforceLivePolicy resolves an unreachable dependency under the policy. Under
// the default policy it fails, naming the service and the cause. Under explicit
// offline mode it skips, naming the dependency and the opt-in. Neither path
// returns: t.Skip and t.Fatal both terminate the calling goroutine.
func enforceLivePolicy(t *testing.T, dependency, endpoint string, cause error) {
	t.Helper()
	p := currentLivePolicy(t)
	if p.offline {
		t.Skipf("%s is unreachable at %s (%v); %s=%s is set, so this test is skipped and claims no coverage of it",
			dependency, endpoint, cause, testModeEnv, offlineTestMode)
	}
	t.Fatalf("%s is unreachable at %s (%v). This package tests running services, so unavailable "+
		"infrastructure is a failure by default: a gate that cannot reach %s has verified nothing. "+
		"Start the stack and re-run, or set %s=%s to skip deliberately.",
		dependency, endpoint, cause, dependency, testModeEnv, offlineTestMode)
}

// requireLivePostgres returns an admin connection to live PostgreSQL, applying
// the policy when it cannot be established. It replaces a helper that skipped
// unconditionally, which let this suite report coverage it had not performed.
func requireLivePostgres(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err != nil {
			_ = db.Close()
		}
	}
	if err != nil {
		enforceLivePolicy(t, "PostgreSQL", redactDSN(dsn), err)
		// Unreachable: enforceLivePolicy skips or fails. Asserted rather than
		// assumed, so a future change that made it fall through could not
		// hand a nil *sql.DB to a caller.
		t.Fatalf("unreachable: enforceLivePolicy must terminate the test")
	}
	recordLiveContact("PostgreSQL")
	return db
}

// TestReleaseGateRequiresLiveInfrastructure is the third clause of the policy: a
// run that claims to be a gate must have reached live infrastructure. It skips
// outside a gate run, following the AUSTRO_AI_LIVE_TEST convention already used
// for the opt-in provider test.
func TestReleaseGateRequiresLiveInfrastructure(t *testing.T) {
	if strings.TrimSpace(os.Getenv(requireLiveEnv)) != "1" {
		t.Skipf("%s is not set, so this run is not a release gate; the live-contact assertion applies only to a gate",
			requireLiveEnv)
	}
	snapshot := liveContactSnapshot()
	var missing []string
	for _, dep := range requiredLiveContact {
		if snapshot[dep] == 0 {
			missing = append(missing, dep)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s=1 but no live contact was recorded for %s (contacts: %v). A release gate that "+
			"reached no live dependency verified nothing and must not pass.",
			requireLiveEnv, strings.Join(missing, ", "), snapshot)
	}
}
