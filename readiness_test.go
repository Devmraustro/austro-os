package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"austro-os/internal/config"
	"austro-os/internal/health"
)

// TestReadinessResponseShape pins the wire contract of /health/ready.
//
// Two things have to hold at once, and the second is easy to break while
// satisfying the first. The body must keep its historical shape â€” "ready" as the
// leading boolean â€” so the deployment scripts that match on it keep working. And
// it must stop being unconditional: a response that always says ready cannot
// distinguish a deployment whose dispatcher is running from one that has none.
func TestReadinessResponseShape(t *testing.T) {
	cases := []struct {
		name       string
		report     health.Report
		wantStatus int
		wantReady  bool
		wantReason string
	}{
		{
			name:       "no companions required",
			report:     health.Report{Ready: true, Required: []string{}},
			wantStatus: http.StatusOK,
			wantReady:  true,
		},
		{
			name: "declared companion running",
			report: health.Report{
				Ready:    true,
				Required: []string{"dispatcher"},
				Companions: []health.RoleStatus{
					{Role: "dispatcher", Instances: 1, LastSeen: time.Now().UTC().Truncate(time.Second)},
				},
			},
			wantStatus: http.StatusOK,
			wantReady:  true,
		},
		{
			name: "declared companion absent",
			report: health.Report{
				Ready:      false,
				Reason:     "companion-absent",
				Required:   []string{"dispatcher"},
				Companions: []health.RoleStatus{{Role: "dispatcher"}},
			},
			// 503 is the load-bearing part: a platform supervisor that only
			// ever looks at the status code must still see this.
			wantStatus: http.StatusServiceUnavailable,
			wantReady:  false,
			wantReason: "companion-absent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeReadiness(w, tc.report)

			require.Equal(t, tc.wantStatus, w.Code)
			require.Equal(t, "application/json", w.Header().Get("Content-Type"))

			var body struct {
				Ready  bool   `json:"ready"`
				Status string `json:"status"`
				Reason string `json:"reason"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, tc.wantReady, body.Ready)
			require.Equal(t, tc.wantReason, body.Reason)
			require.NotEmpty(t, body.Status, "the status string must always be present")

			if tc.wantReady {
				require.Equal(t, "ok", body.Status)
			} else {
				require.Equal(t, "degraded", body.Status)
			}
		})
	}
}

// TestReadinessBodyStartsWithTheReadyField is the compatibility assertion,
// stated directly because it is the kind of thing that breaks silently: the
// scripts in deploy/ match on the body text, so reordering the keys would stop
// them from matching without failing any Go test.
func TestReadinessBodyStartsWithTheReadyField(t *testing.T) {
	w := httptest.NewRecorder()
	writeReadiness(w, health.Report{Ready: true})

	require.Contains(t, w.Body.String(), `{"ready":true`,
		"the body must open with the historical ready field for the existing matchers")
}

// TestReadinessAlwaysEchoesTheRequiredList keeps "no companion needed" and
// "companion needed and missing" distinguishable from the response alone. Without
// the empty list, a reader cannot tell a single-process deployment from one whose
// companions list was never evaluated.
func TestReadinessAlwaysEchoesTheRequiredList(t *testing.T) {
	w := httptest.NewRecorder()
	writeReadiness(w, health.Report{Ready: true, Required: nil})

	var body struct {
		Required []string `json:"requires"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotNil(t, body.Required, "requires must be present and empty, not omitted")
	require.Empty(t, body.Required)
}

// TestReadinessReportsLastSeenInUTCAndOmitsItWhenNeverRun keeps the timestamp
// unambiguous across hosts. A companion runs on a different machine, and a local
// timestamp with no zone is read differently by every reader; a companion that
// has never run has no timestamp at all, and a zero time serialised as
// "0001-01-01T00:00:00Z" would read as data rather than as absence.
func TestReadinessReportsLastSeenInUTCAndOmitsItWhenNeverRun(t *testing.T) {
	zone := time.FixedZone("UTC+7", 7*3600)
	seen := time.Date(2026, 9, 29, 19, 0, 0, 0, zone)

	w := httptest.NewRecorder()
	writeReadiness(w, health.Report{
		Ready:    true,
		Required: []string{"dispatcher"},
		Companions: []health.RoleStatus{
			{Role: "dispatcher", Instances: 1, LastSeen: seen},
			{Role: "worker"},
		},
	})

	var body struct {
		Companions []struct {
			Role      string `json:"role"`
			LastSeen  string `json:"last_seen"`
			Instances int    `json:"instances"`
		} `json:"companions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Companions, 2)
	require.Equal(t, "2026-09-29T12:00:00Z", body.Companions[0].LastSeen, "must be normalised to UTC")
	require.Equal(t, 1, body.Companions[0].Instances)
	require.Empty(t, body.Companions[1].LastSeen, "a companion that never ran has no timestamp")
}

// readinessDepsIsDrivenByTheDeclaration is the regression test for the
// dependency leak that made a single-process deployment depend on a database
// round trip it had no reason to make.
//
// The evaluator used to be handed both roles unconditionally, with the worker
// marked optional. The consequence was not "a little extra information": when
// the heartbeat query failed the evaluator returned an error, the endpoint
// answered 503, and a deployment that declared no companions at all was taken
// out of rotation over a table it never claimed to read. Backward
// compatibility with the historical unconditional 200 only holds while an
// undeclared role is never checked.
//
// The assertion is on the dependency list rather than on the queries it
// produces, because the evaluator stops at the first failing check: with a
// reader that refuses everything, a two-role declaration can only ever be
// observed making one query. Testing the list is also the more direct
// statement of the rule.
func TestReadinessDepsAreDrivenByTheDeclaration(t *testing.T) {
	cases := []struct {
		name       string
		companions string
		wantRoles  []string
	}{
		{name: "nothing declared produces no dependency", companions: ""},
		{name: "dispatcher declared", companions: "dispatcher", wantRoles: []string{"dispatcher"}},
		{name: "worker alone is honoured", companions: "worker", wantRoles: []string{"worker"}},
		{name: "both declared", companions: "dispatcher,worker", wantRoles: []string{"dispatcher", "worker"}},
		{
			name:       "order case and padding do not change the set",
			companions: "  Worker , DISPATCHER  ",
			wantRoles:  []string{"worker", "dispatcher"},
		},
		{name: "a repeated role is one dependency", companions: "dispatcher,dispatcher", wantRoles: []string{"dispatcher"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Load reads the environment without validating, which is the point:
			// building the Config through the real loader keeps the parsing path
			// under test too. Validation is exercised in the config package.
			t.Setenv("AUSTRO_PROCESS_ROLE", config.ProcessRoleAPI)
			t.Setenv("AUSTRO_EXPECT_COMPANION_PROCESSES", tc.companions)
			cfg := config.Load()

			deps := readinessDeps(cfg)
			roles := make([]string, 0, len(deps))
			for _, d := range deps {
				// Every dependency the entrypoint builds is required. A
				// declared-but-advisory role is not a state this deployment
				// model has: naming a role is a claim the process cannot serve
				// traffic without it.
				require.True(t, d.Required, "role %q must be required", d.Role)
				roles = append(roles, d.Role)
			}
			require.ElementsMatch(t, tc.wantRoles, roles)
			if len(tc.wantRoles) == 0 {
				require.Empty(t, deps, "a single-process deployment must check nothing")
			}
		})
	}
}

// TestSingleProcessReadinessSurvivesAnUnusableDatabase is the operational form
// of the same regression, and the one that would have been observed in
// production.
//
// The reader refuses every query. A deployment that declares no companions must
// still answer ready, because it never asked the question the reader would
// refuse to answer. If this regresses, an unrelated database outage takes every
// single-process site out of rotation with a companion-absent verdict it did
// not earn.
func TestSingleProcessReadinessSurvivesAnUnusableDatabase(t *testing.T) {
	t.Setenv("AUSTRO_PROCESS_ROLE", config.ProcessRoleAPI)
	t.Setenv("AUSTRO_EXECUTION_DISPATCH_MODE", config.ExecutionDispatchModeOff)
	t.Setenv("AUSTRO_EXPECT_COMPANION_PROCESSES", "")
	cfg := config.Load()

	reader := &failingLivenessReader{}
	evaluator := health.NewEvaluator(reader, readinessDeps(cfg), cfg.HeartbeatStaleAfter)

	report, err := evaluator.Evaluate(context.Background())
	require.NoError(t, err)
	require.True(t, report.Ready)
	require.Empty(t, report.Required)
	require.Empty(t, reader.roles, "no declared role means no query, so no database dependency")

	w := httptest.NewRecorder()
	writeReadiness(w, report)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"requires":[]`,
		"the response must say it checked nothing rather than omitting the key")
}

// TestDeclaredCompanionThatCannotBeCheckedIsNotReady is the other half: a role
// the deployment did name, read through a database that cannot answer, must
// fail closed. The fix for the leak above must not have turned into a softer
// verdict for the case that matters.
func TestDeclaredCompanionThatCannotBeCheckedIsNotReady(t *testing.T) {
	t.Setenv("AUSTRO_PROCESS_ROLE", config.ProcessRoleAPI)
	t.Setenv("AUSTRO_EXPECT_COMPANION_PROCESSES", "dispatcher")
	cfg := config.Load()

	evaluator := health.NewEvaluator(&failingLivenessReader{}, readinessDeps(cfg), cfg.HeartbeatStaleAfter)
	report, err := evaluator.Evaluate(context.Background())
	require.Error(t, err, "an unknown state must not be reported as a healthy one")
	require.False(t, report.Ready)
	require.Equal(t, "liveness-unavailable", report.Reason)
	require.Equal(t, []string{"dispatcher"}, report.Required)
}

// failingLivenessReader refuses every liveness query, standing in for a
// database that cannot answer, and records which roles were asked about.
type failingLivenessReader struct {
	roles []string
}

func (f *failingLivenessReader) Liveness(_ context.Context, role string, _ time.Duration) (health.RoleStatus, error) {
	f.roles = append(f.roles, role)
	return health.RoleStatus{Role: role}, errors.New("liveness unavailable")
}

// TestReadiness503CarriesTheFullDocumentedBody is the wire-level half of the
// contract.
//
// When the check cannot be completed, the response has to have the same shape
// as every other readiness response, because that response is the one an
// operator reads while deciding whether to pull a host. A 503 that quietly
// dropped status and requires would tell them less exactly when they need it
// most, and the documented schema would be describing a body the server never
// sends.
func TestReadiness503CarriesTheFullDocumentedBody(t *testing.T) {
	t.Setenv("AUSTRO_PROCESS_ROLE", config.ProcessRoleAPI)
	t.Setenv("AUSTRO_EXPECT_COMPANION_PROCESSES", "dispatcher")
	cfg := config.Load()

	evaluator := health.NewEvaluator(&failingLivenessReader{}, readinessDeps(cfg), cfg.HeartbeatStaleAfter)
	report, err := evaluator.Evaluate(context.Background())
	require.Error(t, err)

	w := httptest.NewRecorder()
	writeReadiness(w, report)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	for _, key := range []string{"ready", "status", "reason", "requires"} {
		require.Contains(t, raw, key, "the 503 body must satisfy the documented schema")
	}

	var body struct {
		Ready    bool     `json:"ready"`
		Status   string   `json:"status"`
		Reason   string   `json:"reason"`
		Required []string `json:"requires"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.False(t, body.Ready)
	require.Equal(t, "degraded", body.Status)
	require.Equal(t, "liveness-unavailable", body.Reason)
	require.Equal(t, []string{"dispatcher"}, body.Required)
}
