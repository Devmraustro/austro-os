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
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// isInfrastructureUnavailable reports whether err means the request never
// reached a service, as opposed to a service that was reached and answered.
//
// This is the discriminator the offline policy is allowed to act on, and it is
// deliberately one-sided. Only a network-level failure qualifies:
//
//   - *net.DNSError  the name did not resolve, so nothing was listening
//   - *net.OpError    the connection was refused, reset, or timed out
//
// Everything else is a service that answered and is therefore never eligible
// for a skip. That deliberately excludes:
//
//   - HTTP status codes. callAuthJSON returns (status, body, nil) for every
//     status, so a 401, 403, 409, 429 or 500 arrives with a nil error and can
//     never reach this function as an error at all. The exclusion is structural
//     rather than a matter of matching strings.
//   - *amqp.Error, which the broker returns for a protocol-level refusal such
//     as ACCESS_REFUSED on bad credentials. The broker was reached; a wrong
//     password is a configuration fault, not a missing service.
//   - *pgconn.PgError, which PostgreSQL returns for SQL-level faults. The server
//     was reached and rejected the statement.
//
// The asymmetry is the whole point. Treating a reachable service's refusal as
// "infrastructure unavailable" is how a green run gets produced from a
// deployment that is misconfigured, so anything ambiguous fails.
func isInfrastructureUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}

// requireLiveDependency applies the live policy to a failure that is not
// PostgreSQL's, using the same clause-1/clause-2 rule requireLivePostgres
// applies: skip under explicit offline mode, fail otherwise.
//
// When the failure is not a network-level one the test fails even under offline
// mode, because a service that answered is not an absent service. The message
// says so explicitly, so a skip count that silently absorbed a real refusal is
// visible in the output rather than inferred from a smaller number.
func requireLiveDependency(t *testing.T, dependency, endpoint string, err error) {
	t.Helper()
	if isInfrastructureUnavailable(err) {
		enforceLivePolicy(t, dependency, endpoint, err)
		// Unreachable: enforceLivePolicy skips or fails.
		t.Fatalf("unreachable: enforceLivePolicy must terminate the test")
	}
	t.Fatalf("%s answered at %s but the request failed, which is not an absent service and is never a skip: %v. "+
		"A refusal from a reachable service (a wrong password, a denied grant, a rejected statement) is a "+
		"configuration or product fault: fix it rather than setting %s=%s.",
		dependency, endpoint, err, testModeEnv, offlineTestMode)
}

// requireLiveHTTP applies the live policy to a failed HTTP call.
//
// The error is only ever non-nil when the exchange did not complete: the
// request could not be built or sent, or the connection failed or was dropped
// mid-body. A completed exchange returns a status and a nil error, so routing
// this path through the policy cannot convert an HTTP status into a skip.
func requireLiveHTTP(t *testing.T, err error) {
	t.Helper()
	requireLiveDependency(t, "the API", getEnv().apiURL, err)
}

// liveDependencyForError attributes a failure to the dependency that produced
// it. The shared-session initializers talk to both the API and PostgreSQL, so a
// skip must name the one that actually failed instead of guessing.
func liveDependencyForError(err error) (name, endpoint string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "PostgreSQL", getEnv().postgresDSN
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "the API", getEnv().apiURL
	}
	return "the live stack", getEnv().apiURL
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
