package austro_os_test

// Regression tests for the live-infrastructure policy in live_policy_test.go.
//
// The policy has three clauses and each is asserted here:
//
//	1. default            -> unavailable infrastructure FAILS
//	2. explicit offline    -> unavailable infrastructure SKIPS, with a reason
//	3. gate set            -> offline mode is refused, and no live contact FAILS
//
// The skip/fail distinction cannot be observed from inside a test -- a skipped
// subtest and a passed one both report success to t.Run -- so the behavioural
// cases run the compiled test binary against an endpoint that cannot resolve
// and assert on its exit status and output. None of them needs live
// infrastructure, because the policy is precisely about what happens when
// infrastructure is absent.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

// policyProbeDSN points at the reserved .invalid TLD (RFC 6761), which never
// resolves, so the probe exercises the real dial path with no service running.
const policyProbeDSN = "postgres://austro:s3cret@austro-postgres.invalid:5432/austro?sslmode=disable"

const policyProbeEnv = "AUSTRO_LIVE_POLICY_PROBE"

// runPolicyProbe re-executes this test binary for one probe case and returns
// its combined output and whether it failed.
func runPolicyProbe(t *testing.T, name string, env ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(), policyProbeEnv+"=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err != nil
}

// TestLivePolicyFailsWhenLiveIsRequiredByDefault is clause 1. With no opt-in,
// an unreachable service must fail the test and name the service, so the failure
// is actionable instead of looking like an unrelated assertion.
func TestLivePolicyFailsWhenLiveIsRequiredByDefault(t *testing.T) {
	if os.Getenv(policyProbeEnv) == "1" {
		requireLivePostgres(t, policyProbeDSN)
		return
	}
	out, failed := runPolicyProbe(t, "TestLivePolicyFailsWhenLiveIsRequiredByDefault")
	require.True(t, failed, "unavailable infrastructure must FAIL by default; output:\n%s", out)
	require.Contains(t, out, "PostgreSQL is unreachable")
	require.Contains(t, out, "austro-postgres.invalid", "the diagnostic must name the endpoint")
	require.Contains(t, out, "AUSTRO_TEST_MODE=offline",
		"the diagnostic must state the opt-in for skipping")
	require.NotContains(t, out, "SKIP",
		"a required-live test must never skip by default")
}

// TestLivePolicySkipsOnlyInExplicitOfflineMode is clause 2. Offline mode is the
// single way to skip, and the reason must name both the dependency and the
// opt-in so the skip cannot be mistaken for coverage.
func TestLivePolicySkipsOnlyInExplicitOfflineMode(t *testing.T) {
	if os.Getenv(policyProbeEnv) == "1" {
		requireLivePostgres(t, policyProbeDSN)
		return
	}
	out, failed := runPolicyProbe(t, "TestLivePolicySkipsOnlyInExplicitOfflineMode", "AUSTRO_TEST_MODE=offline")
	require.False(t, failed, "explicit offline mode must skip, not fail; output:\n%s", out)
	require.Contains(t, out, "--- SKIP")
	require.Contains(t, out, "claims no coverage",
		"a skip must state that it claims no coverage")
	require.Contains(t, out, "AUSTRO_TEST_MODE=offline",
		"a skip must name the opt-in that caused it")
}

// TestLivePolicyRefusesOfflineModeUnderAGate is the first half of clause 3: a
// gate that runs in offline mode is a misconfiguration, and the contradiction is
// a failure rather than a silent downgrade to skips.
func TestLivePolicyRefusesOfflineModeUnderAGate(t *testing.T) {
	if os.Getenv(policyProbeEnv) == "1" {
		requireLivePostgres(t, policyProbeDSN)
		return
	}
	out, failed := runPolicyProbe(t, "TestLivePolicyRefusesOfflineModeUnderAGate",
		"AUSTRO_TEST_MODE=offline", "AUSTRO_REQUIRE_LIVE_TESTS=1")
	require.True(t, failed, "a gate running in offline mode must FAIL; output:\n%s", out)
	require.Contains(t, out, "contradicts")
	require.NotContains(t, out, "--- SKIP",
		"the contradictory configuration must not resolve in favour of skipping")
}

// TestLivePolicyNeverInfersOfflineFromAnythingElse is the safety property that
// makes clause 1 hold. Offline mode is enabled by one exact value: a near miss
// must leave the strict default in place, or a typo in CI would quietly convert
// a release gate into a suite of skips.
func TestLivePolicyNeverInfersOfflineFromAnythingElse(t *testing.T) {
	for _, mode := range []string{"", "off", "OFFLINE_MAYBE", "0", "true", "yes", "skip", "offline "} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Setenv(testModeEnv, mode)
			t.Setenv(requireLiveEnv, "")
			p := currentLivePolicy(t)
			if strings.TrimSpace(mode) == offlineTestMode {
				require.True(t, p.offline, "the exact value must enable offline mode")
			} else {
				require.False(t, p.offline,
					"only %q may enable offline mode; %q must keep the strict default", offlineTestMode, mode)
				require.False(t, p.requireLive)
			}
		})
	}
}

// TestLivePolicyOfflineModeIsCaseInsensitive confirms the documented value is
// accepted regardless of case, so behaviour does not depend on shell casing.
func TestLivePolicyOfflineModeIsCaseInsensitive(t *testing.T) {
	t.Setenv(requireLiveEnv, "")
	for _, mode := range []string{"offline", "OFFLINE", "Offline", " offline "} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(testModeEnv, mode)
			require.True(t, currentLivePolicy(t).offline)
		})
	}
}

// TestLivePolicyGateIsRecognisedOnlyByOne covers the gate flag itself: any other
// value leaves the run ungated rather than pretending to be a gate.
func TestLivePolicyGateIsRecognisedOnlyByOne(t *testing.T) {
	t.Setenv(testModeEnv, "")
	for _, v := range []string{"", "0", "true", "yes", "2"} {
		t.Run("gate="+v, func(t *testing.T) {
			t.Setenv(requireLiveEnv, v)
			require.False(t, currentLivePolicy(t).requireLive,
				"only 1 marks a release gate")
		})
	}
	t.Run("gate=1", func(t *testing.T) {
		t.Setenv(requireLiveEnv, "1")
		require.True(t, currentLivePolicy(t).requireLive)
	})
}

// The tests below pin the offline policy's safety property: the opt-in may only
// ever absorb a service that was never reached, never a service that answered.
//
// The classifier itself is asserted directly, because it is a pure function of
// an error value. The behavioural cases then run the compiled binary, since a
// skipped subtest and a failed one both report success to t.Run and so cannot be
// distinguished from the inside. None of this needs live infrastructure, and
// none of it needs a running API: the discriminator depends only on the error's
// type, which is why it can be verified with the stack down.

// policyModeEnv selects which probe case the child process runs. It is separate
// from policyProbeEnv because the existing probe harness keys off the latter.
const policyModeEnv = "AUSTRO_POLICY_MODE"

// TestInfrastructureUnavailableClassifier pins the one-sided rule: only a
// network-level failure counts as an absent service.
func TestInfrastructureUnavailableClassifier(t *testing.T) {
	t.Run("a DNS failure is an absent service", func(t *testing.T) {
		require.True(t, isInfrastructureUnavailable(&url.Error{
			Op: "Get", URL: unreachableAPI, Err: &net.DNSError{Err: "no such host", Name: "austro-api.invalid"},
		}))
	})

	t.Run("a refused connection is an absent service", func(t *testing.T) {
		require.True(t, isInfrastructureUnavailable(&net.OpError{
			Op: "dial", Net: "tcp", Err: errors.New("connection refused"),
		}))
	})

	t.Run("the rule survives wrapping", func(t *testing.T) {
		wrapped := fmt.Errorf("POST /api/auth/login against %s: %w", unreachableAPI, &url.Error{
			Op: "Post", URL: unreachableAPI, Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")},
		})
		require.True(t, isInfrastructureUnavailable(wrapped),
			"the initializer wraps the transport error, so the rule must unwrap")
	})

	t.Run("a broker refusal is a reachable service answering", func(t *testing.T) {
		require.False(t, isInfrastructureUnavailable(&amqp.Error{
			Code: 403, Reason: "ACCESS_REFUSED - login was refused", Server: true,
		}), "a wrong password reached the broker; it is a configuration fault, not an absent service")
	})

	t.Run("a SQL-level fault is a reachable server answering", func(t *testing.T) {
		require.False(t, isInfrastructureUnavailable(&pgconn.PgError{Code: "42501"}),
			"permission denied came from PostgreSQL, so the server was reached")
	})

	t.Run("an unrecognised error is never an absent service", func(t *testing.T) {
		require.False(t, isInfrastructureUnavailable(errors.New("connection refused")),
			"the rule matches on type, not on message text, so a plain error fails closed")
		require.False(t, isInfrastructureUnavailable(nil))
	})
}

// TestHTTPErrorStatusIsNeverOfflineSkippable is the property that matters most,
// and it is structural rather than a matter of classification: callAuthJSON
// returns a nil error for every status, so an HTTP status cannot reach the
// policy as an error in the first place.
func TestHTTPErrorStatusIsNeverOfflineSkippable(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict,
		http.StatusTooManyRequests, http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"refused"}`))
			}))
			defer srv.Close()

			gotStatus, body, err := callAuthJSON(srv.URL, http.MethodGet, "/api/whoami", nil, "")
			require.NoError(t, err,
				"a %d from a reachable server is a completed exchange, so it must not be an error", status)
			require.Equal(t, status, gotStatus)
			require.NotEmpty(t, body)
			require.False(t, isInfrastructureUnavailable(err),
				"and it can never be classified as an absent service")
		})
	}
}

// TestOfflinePolicySkipsOnlyAbsentServices is the behavioural half: offline mode
// absorbs a service that was never reached, the default policy still fails it,
// and a reachable service's refusal is refused a skip either way.
func TestOfflinePolicySkipsOnlyAbsentServices(t *testing.T) {
	switch os.Getenv(policyModeEnv) {
	case "absent-offline":
		t.Setenv(testModeEnv, offlineTestMode)
		_, _, err := callAuthJSON(unreachableAPI, http.MethodGet, "/api/whoami", nil, "")
		require.Error(t, err)
		requireLiveHTTP(t, err) // must skip
	case "absent-default":
		t.Setenv(testModeEnv, "")
		_, _, err := callAuthJSON(unreachableAPI, http.MethodGet, "/api/whoami", nil, "")
		require.Error(t, err)
		requireLiveHTTP(t, err) // must fail
	case "answered":
		t.Setenv(testModeEnv, offlineTestMode)
		requireLiveDependency(t, "the API", "http://austro-api.invalid:8080",
			&amqp.Error{Code: 403, Reason: "ACCESS_REFUSED", Server: true}) // must fail
	default:
		t.Run("offline mode absorbs an absent service", func(t *testing.T) {
			out, _ := runPolicyProbe(t, t.Name(), policyModeEnv+"=absent-offline")
			require.Contains(t, out, "claims no coverage of it",
				"the skip must name the dependency and the opt-in, and claim no coverage")
		})
		t.Run("the default policy still fails an absent service", func(t *testing.T) {
			out, _ := runPolicyProbe(t, t.Name(), policyModeEnv+"=absent-default")
			require.Contains(t, out, "unavailable infrastructure is a failure by default",
				"without the opt-in, unavailable infrastructure must remain a failure")
		})
		t.Run("offline mode refuses to absorb a reachable service", func(t *testing.T) {
			out, _ := runPolicyProbe(t, t.Name(), policyModeEnv+"=answered")
			require.Contains(t, out, "which is not an absent service and is never a skip",
				"a refusal from a reachable service must fail even under the opt-in")
		})
	}
}

// TestLiveContactLedgerRecordsReachableDependencies is the second half of clause
// 3, at unit level: the ledger the gate asserts on must actually record.
func TestLiveContactLedgerRecordsReachableDependencies(t *testing.T) {
	// Start from a ledger this test owns.
	//
	// liveContacts is a process-wide counter by design: the gate reads it after
	// every test in the package has run. That makes it the wrong thing to assert
	// "is empty" about from inside the package, because every other live test
	// that reached PostgreSQL before this one has already written to it. The
	// original assertion passed only when this test happened to run first,
	// which made the outcome a function of declaration order rather than of the
	// behaviour under test. Swap the map out for the duration instead: the
	// property being verified is that recording increments and snapshotting
	// copies, both of which are independent of what anything else recorded.
	liveContactMu.Lock()
	saved := liveContacts
	liveContacts = map[string]int{}
	liveContactMu.Unlock()
	t.Cleanup(func() {
		liveContactMu.Lock()
		liveContacts = saved
		liveContactMu.Unlock()
	})

	require.Empty(t, liveContactSnapshot(), "a ledger this test has not written to must be empty")

	recordLiveContact("PostgreSQL")
	recordLiveContact("PostgreSQL")
	recordLiveContact("API")

	snapshot := liveContactSnapshot()
	require.Equal(t, 2, snapshot["PostgreSQL"], "contacts must be counted, not deduplicated")
	require.Equal(t, 1, snapshot["API"])

	// The snapshot must be a copy: a caller mutating it cannot corrupt the ledger.
	snapshot["PostgreSQL"] = 99
	require.Equal(t, 2, liveContactSnapshot()["PostgreSQL"])
}

// TestLivePolicyRequiredContactListIsNonEmpty guards the gate against being
// satisfied vacuously by an empty requirement list.
func TestLivePolicyRequiredContactListIsNonEmpty(t *testing.T) {
	require.NotEmpty(t, requiredLiveContact)
	for _, dep := range requiredLiveContact {
		require.NotEmpty(t, strings.TrimSpace(dep))
	}
}

// TestNoInfrastructureSkipsOutsideThePolicy is the consistency guard. It scans
// the package and fails if any file other than live_policy_test.go skips on
// infrastructure-availability wording, which is how the previous unconditional
// skip was allowed to hide missing RLS coverage. Skips for inapplicable
// conditions -- a chain too short to page, a missing release artifact -- are
// not infrastructure skips and are left alone.
func TestNoInfrastructureSkipsOutsideThePolicy(t *testing.T) {
	root, err := locateRepoRoot()
	require.NoError(t, err, "the repo root must be locatable for the package scan")

	entries, err := os.ReadDir(filepath.Join(root, "tests"))
	require.NoError(t, err)

	infraWording := regexp.MustCompile(`(?i)unavailable|unreachable|no such host|dial tcp|connection refused|deadline exceeded`)
	skipCall := regexp.MustCompile(`t\.Skipf?\(`)

	var offenders []string
	skipsSeen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(filepath.Join(root, "tests", name))
		require.NoError(t, readErr)
		for i, line := range strings.Split(string(src), "\n") {
			if !skipCall.MatchString(line) {
				continue
			}
			skipsSeen++
			if name == "live_policy_test.go" {
				continue // the policy helper is where an infra skip belongs
			}
			if infraWording.MatchString(line) {
				offenders = append(offenders, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}

	require.Empty(t, offenders,
		"infrastructure availability must be handled by the policy in live_policy_test.go, not by skipping inline; "+
			"found:\n%s", strings.Join(offenders, "\n"))
	t.Logf("scanned %d skip call(s) across tests/", skipsSeen)
}
