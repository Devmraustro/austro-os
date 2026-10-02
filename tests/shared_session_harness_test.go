package austro_os_test

// Regression tests for the shared live-session harness.
//
// The defect these guard: the founder and workspace session helpers ran
// t-dependent code inside a sync.Once body. When the live stack was
// unreachable, require.NoError inside that body called FailNow, which unwinds
// the goroutine with runtime.Goexit. sync.Once marks itself done from a defer
// (runtime/sync/once.go), so it recorded the half-run initializer as complete.
// The error variable was never assigned, so require.NoError saw nil and passed
// silently, and every later test in the package failed on
// "Should NOT be empty" against a token no test ever received -- a diagnostic
// pointing at an assertion rather than at the missing service.
//
// Every test here runs without live infrastructure, using endpoints that
// cannot resolve or that serve a fixed response, so a regression is caught by
// an ordinary offline `go test ./tests/` run instead of only on a machine where
// the stack happens to be down.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// unreachableAPI is in the .invalid TLD, which RFC 6761 reserves and which
// therefore never resolves. Using it exercises the real http.Client and the
// real DNS failure path with no live service and no network dependency.
const unreachableAPI = "http://austro-api.invalid:8080"

const unreachableDSN = "postgres://austro:s3cret@austro-postgres.invalid:5432/austro?sslmode=disable"

var errSessionInitFailed = errors.New("dial tcp: lookup postgres: no such host")

// TestSharedSessionOnceNeverCachesHalfInitializedState holds the invariant the
// session helpers exist to provide: an initializer that fails leaves a recorded
// error, so every later caller reports that error rather than an empty value.
//
// The first subtest pins down the original defect. A body that terminates
// without assigning its result is still recorded as complete, which is why the
// nil error satisfied require.NoError while the empty token survived.
func TestSharedSessionOnceNeverCachesHalfInitializedState(t *testing.T) {
	t.Run("an aborted body is still recorded as complete", func(t *testing.T) {
		var (
			once sync.Once
			tok  string
			err  error
		)
		abort := func() {
			once.Do(func() {
				// Stands in for t.Fatal -> runtime.Goexit, which unwinds the
				// goroutine before err or tok is ever assigned.
				panic("simulated Fatal inside the once body")
			})
		}
		require.Panics(t, abort, "the simulated Fatal propagates to the caller")

		bodyRan := false
		once.Do(func() { bodyRan = true })
		require.False(t, bodyRan,
			"sync.Once recorded the aborted body as complete, so it is never retried")
		require.NoError(t, err,
			"this nil error is exactly what require.NoError silently accepted")
		require.Empty(t, tok,
			"and this empty token was the only symptom later tests could see")
	})

	t.Run("a returned error reaches every caller unchanged", func(t *testing.T) {
		var (
			once    sync.Once
			tok     string
			initErr error
		)
		caller := func() (string, error) {
			once.Do(func() {
				tok, initErr = "", errSessionInitFailed
			})
			return tok, initErr
		}

		for i := 1; i <= 3; i++ {
			got, err := caller()
			require.ErrorIs(t, err, errSessionInitFailed,
				"caller %d must see the original initializer error", i)
			require.Empty(t, got, "caller %d must not receive a token", i)
			msg := sharedTokenFailure("workspace", err, map[string]string{"adminA": got})
			require.Contains(t, msg, errSessionInitFailed.Error(),
				"caller %d: the diagnostic must carry the cause, not the empty token", i)
		}
	})
}

// TestSharedTokenFailureNamesTheCause pins the diagnostic contract. The whole
// point of the fix is that a failure says what went wrong; "Should NOT be
// empty" said nothing.
func TestSharedTokenFailureNamesTheCause(t *testing.T) {
	t.Run("an initializer error is reported verbatim", func(t *testing.T) {
		msg := sharedTokenFailure("founder",
			errors.New("logging the founder in: POST /api/auth/login against http://api:8080: dial tcp: lookup api: no such host"),
			nil)
		require.Contains(t, msg, "shared founder session could not be established")
		require.Contains(t, msg, "lookup api: no such host")
		require.NotContains(t, msg, "Should NOT be empty")
	})

	t.Run("a healthy session produces no message", func(t *testing.T) {
		require.Empty(t, sharedTokenFailure("founder", nil, map[string]string{"token": "abc"}))
		require.Empty(t, sharedTokenFailure("workspace", nil, map[string]string{}),
			"a session with no tokens to check is not itself a failure")
	})

	t.Run("an empty token with no error is called a harness fault", func(t *testing.T) {
		msg := sharedTokenFailure("workspace", nil, map[string]string{
			"adminA": "", "memberA": "ok", "adminB": "",
		})
		require.Contains(t, msg, "adminA, adminB",
			"every missing token must be named, in a stable order")
		require.Contains(t, msg, "test-harness fault",
			"an empty token without an error cannot be a product failure")
		require.NotContains(t, msg, "memberA", "a populated token is not a fault")
	})
}

// TestRequireSharedTokenTerminatesTheCaller proves the helper actually fails
// the test that asked for an unusable session, and that it fails with the
// cause. The probe runs in a subprocess because a failed subtest cannot be
// observed from the inside without failing the enclosing test as well.
func TestRequireSharedTokenTerminatesTheCaller(t *testing.T) {
	const probeEnv = "AUSTRO_SHARED_SESSION_FATAL_PROBE"
	if os.Getenv(probeEnv) == "1" {
		// Never returns: the session is unusable, so the test must fail.
		requireSharedToken(t, "founder", errors.New("lookup api: no such host"), nil)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRequireSharedTokenTerminatesTheCaller$", "-test.v")
	cmd.Env = append(os.Environ(), probeEnv+"=1")
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr,
		"the probe must fail the test; output was:\n%s", out)
	require.Contains(t, string(out),
		"shared founder session could not be established: lookup api: no such host",
		"the failure must name the cause")
	require.NotContains(t, string(out), "Should NOT be empty",
		"the misleading empty-token assertion must not reappear")
	require.NotContains(t, string(out), "PASS")
}

// TestCallAuthJSONReportsUnreachableAPI covers the primitive the founder
// initializer is built on: an unreachable endpoint must produce an error naming
// the endpoint, never a zero status with an empty body.
func TestCallAuthJSONReportsUnreachableAPI(t *testing.T) {
	status, body, err := callAuthJSON(unreachableAPI, http.MethodPost, "/api/auth/login", nil, "")
	require.Error(t, err, "an unreachable API must return an error, not an empty response")
	require.Zero(t, status)
	require.Nil(t, body)
	require.Contains(t, err.Error(), "/api/auth/login")
	require.Contains(t, err.Error(), "austro-api.invalid",
		"the diagnostic must name the endpoint that could not be reached")
	require.NotContains(t, err.Error(), "Should NOT be empty")
}

// TestInitAuditFounderSessionSurfacesSetupFailure proves the original
// symptom -- a caller with an empty token and no error -- can no longer occur
// when the API is unreachable.
func TestInitAuditFounderSessionSurfacesSetupFailure(t *testing.T) {
	tok, err := initAuditFounderSession(unreachableAPI, "founder", "founder-password")
	require.Error(t, err)
	require.Empty(t, tok,
		"an initializer must never return an empty token alongside a nil error")
	require.Contains(t, err.Error(), "bootstrapping the founder session")
	require.Contains(t, err.Error(), "austro-api.invalid")
}

// TestInitAuditFounderSessionContract drives the initializer against a fixed
// local response to check each branch. The server here stands in for the login
// endpoint's *shape*; it asserts nothing about the live API, which the live
// tests cover against the running stack.
func TestInitAuditFounderSessionContract(t *testing.T) {
	t.Run("an idempotent bootstrap followed by a login yields a token", func(t *testing.T) {
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/auth/bootstrap" {
				// An already-initialized organization answers 409; the login
				// must still be attempted.
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"already bootstrapped"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"founder-token","token_type":"Bearer"}`))
		}))
		defer srv.Close()

		tok, err := initAuditFounderSession(srv.URL, "founder", "founder-password")
		require.NoError(t, err)
		require.Equal(t, "founder-token", tok)
		require.Equal(t, []string{"/api/auth/bootstrap", "/api/auth/login"}, paths,
			"a 409 bootstrap must not stop the login")
	})

	t.Run("a server error from bootstrap is reported", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"database unavailable"}`))
		}))
		defer srv.Close()

		tok, err := initAuditFounderSession(srv.URL, "founder", "founder-password")
		require.Error(t, err)
		require.Empty(t, tok)
		require.Contains(t, err.Error(), "bootstrap failed (500)")
		require.Contains(t, err.Error(), "database unavailable")
	})

	t.Run("a rejected login is reported", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/auth/bootstrap" {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid credentials"}`))
		}))
		defer srv.Close()

		tok, err := initAuditFounderSession(srv.URL, "founder", "wrong-password")
		require.Error(t, err)
		require.Empty(t, tok)
		require.Contains(t, err.Error(), "founder login failed (401)")
		require.Contains(t, err.Error(), "invalid credentials")
	})

	t.Run("a 200 without an access token is an error, not an empty session", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/auth/bootstrap" {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			// A rate limiter or proxy can answer 200 with an error document.
			// That must not be mistaken for a session.
			_, _ = w.Write([]byte(`{"token_type":"Bearer","error":"rate limited"}`))
		}))
		defer srv.Close()

		tok, err := initAuditFounderSession(srv.URL, "founder", "founder-password")
		require.Error(t, err, "a tokenless 200 must not become a cached empty session")
		require.Empty(t, tok)
		require.Contains(t, err.Error(), "no access_token")
	})

	t.Run("a malformed body is reported", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/auth/bootstrap" {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_, _ = w.Write([]byte(`not json at all`))
		}))
		defer srv.Close()

		tok, err := initAuditFounderSession(srv.URL, "founder", "founder-password")
		require.Error(t, err)
		require.Empty(t, tok)
		require.Contains(t, err.Error(), "decoding the founder login response")
	})
}

// TestProvisionRbacUsersReportsUnreachablePostgres is the same guarantee for
// the workspace sessions: provisioning failure is returned, and the diagnostic
// names the host without leaking the password.
func TestProvisionRbacUsersReportsUnreachablePostgres(t *testing.T) {
	adminA, memberA, adminB, err := provisionRbacUsers(unreachableDSN)
	require.Error(t, err)
	require.Nil(t, adminA)
	require.Nil(t, memberA)
	require.Nil(t, adminB)
	require.Contains(t, err.Error(), "austro-postgres.invalid",
		"the diagnostic must name the database that could not be reached")
	require.NotContains(t, err.Error(), "s3cret",
		"credentials must never appear in a failure message")
}

// TestInitAuditWorkspaceTokensReportsProvisioningFailure checks the workspace
// initializer surfaces the provisioning error instead of an empty token set.
func TestInitAuditWorkspaceTokensReportsProvisioningFailure(t *testing.T) {
	sessions, err := initAuditWorkspaceTokens(unreachableDSN)
	require.Error(t, err)
	require.Equal(t, rbacSessions{}, sessions)
	require.Contains(t, err.Error(), "austro-postgres.invalid")
	require.NotEmpty(t, sharedTokenFailure("workspace", err, map[string]string{
		"adminA": sessions.adminA, "memberA": sessions.memberA, "adminB": sessions.adminB,
	}))
}

// TestRedactDSNKeepsTheHostDropsThePassword covers the helper used in every
// database diagnostic.
func TestRedactDSNKeepsTheHostDropsThePassword(t *testing.T) {
	got := redactDSN(unreachableDSN)
	require.NotContains(t, got, "s3cret")
	require.NotContains(t, got, "austro:s3cret")
	require.Contains(t, got, "austro-postgres.invalid:5432",
		"the host and port identify which service is missing")
	require.Equal(t, "postgres://austro-postgres.invalid:5432/austro?sslmode=disable", got)
}

// TestAuthJSONWrapperPreservesContract confirms the t-taking wrapper still
// behaves as the 65 existing call sites expect, by exercising it through a
// subtest against a fixed response.
func TestAuthJSONWrapperPreservesContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path})
	}))
	defer srv.Close()

	previous := getEnv().apiURL
	getEnv().apiURL = srv.URL
	t.Cleanup(func() { getEnv().apiURL = previous })

	var (
		status int
		body   []byte
	)
	require.True(t, t.Run("inner", func(t *testing.T) {
		status, body = authJSON(t, http.MethodGet, "/audit/events?limit=5", nil, "token")
	}), "a reachable API must not fail the caller")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(body), "/audit/events")
}

// TestSharedSessionHelpersReportRatherThanCacheEmptyTokens is the end-to-end
// invariant for the two real helpers: when the stack is unreachable they must
// fail on the cause. It reaches them through their own initializers, so it
// holds on a machine with no services running -- which is precisely the
// situation that used to produce 134 unrelated-looking failures.
func TestSharedSessionHelpersReportRatherThanCacheEmptyTokens(t *testing.T) {
	tok, err := initAuditFounderSession(unreachableAPI, "founder", "founder-password")
	msg := sharedTokenFailure("founder", err, map[string]string{"token": tok})
	require.NotEmpty(t, msg, "an unreachable stack must produce a diagnostic")
	require.NotContains(t, msg, "Should NOT be empty")
	require.NotContains(t, msg, "be empty")
	for _, want := range []string{"founder", "austro-api.invalid"} {
		require.True(t, strings.Contains(msg, want),
			fmt.Sprintf("the diagnostic must mention %q; got %q", want, msg))
	}
}
