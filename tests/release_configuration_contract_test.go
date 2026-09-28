package austro_os_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file is the executable form of a drift guard. The execution subsystem
// (dispatcher in the API process, worker in its own process) was fully
// implemented, fully tested against a real broker, and simultaneously absent
// from every deployment artifact in the repository: no compose file, no
// environment template and no documentation mentioned a single AUSTRO_EXECUTION_*
// setting. A production deployment built from those files therefore started with
// the transport defaulted to the offline stub, and with no workspaces configured
// the dispatcher would not have started at all — while every health check,
// readiness probe and monitoring script reported the deployment healthy.
//
// Configuration validation is the last line of that defence, not the first. The
// first line is that the deployment says what it is doing.

// productionDeploymentArtifacts are the files that describe a real deployment.
// Each is expected to state the execution transport contract, because each is
// what an operator copies when standing up a production environment.
var productionDeploymentArtifacts = []string{
	"../.env.example",
	"../docker-compose.production.yml",
	"../deploy/alwaysdata/austro.env.alwaysdata",
	"../deploy/native/austro.env.example",
}

// executionContractVars are the settings an operator must be able to find and
// change without reading Go source. AUSTRO_EXECUTION_QUEUE is here because the
// queue name is the only rendezvous between the two processes: if a deployment
// cannot set it in one place, renaming it means editing source.
var executionContractVars = []string{
	"AUSTRO_EXECUTION_TRANSPORT_BACKEND",
	"AUSTRO_EXECUTION_QUEUE",
	"AUSTRO_EXECUTION_WORKSPACE_IDS",
	"AUSTRO_ALLOW_STUB_BACKENDS",
}

// TestDeploymentArtifactsDeclareTheExecutionTransportContract verifies every
// deployment artifact names each execution setting.
func TestDeploymentArtifactsDeclareTheExecutionTransportContract(t *testing.T) {
	for _, artifact := range productionDeploymentArtifacts {
		t.Run(filepath.Base(artifact), func(t *testing.T) {
			body := readRepoFile(t, artifact)
			for _, name := range executionContractVars {
				require.Containsf(t, body, name,
					"%s does not mention %s: an operator cannot configure what the file never names", artifact, name)
			}
		})
	}
}

// TestProductionEnvironmentTemplatesDoNotShipAnActiveStubBackend verifies the
// production templates do not hand an operator a working configuration that
// runs no real work.
//
// Both templates declare AUSTRO_ENV=production, and production now refuses every
// stub adapter at startup, so shipping "AUSTRO_AI_BACKEND=stub" uncommented was
// never going to boot. The failure this guards is quieter than that crash: an
// operator who copies the template, comments the line out to "fix" the error and
// moves on has silently selected the real default, or has deleted the setting and
// fallen back to a stub that reports success while doing nothing. The line must
// appear as a commented, explicit choice.
func TestProductionEnvironmentTemplatesDoNotShipAnActiveStubBackend(t *testing.T) {
	for _, artifact := range []string{
		"../deploy/alwaysdata/austro.env.alwaysdata",
		"../deploy/native/austro.env.example",
	} {
		t.Run(filepath.Base(artifact), func(t *testing.T) {
			body := readRepoFile(t, artifact)
			require.Contains(t, body, "AUSTRO_ENV=production",
				"%s is a production template and must say so", artifact)

			for _, line := range activeAssignments(t, body) {
				name, value, ok := assignment(line)
				if !ok || !strings.HasSuffix(name, "_BACKEND") {
					continue
				}
				require.NotEqualf(t, "stub", strings.TrimSpace(value),
					"%s ships an active %s=stub. A stub answers every request "+
						"successfully without contacting a real service, so a production "+
						"deployment left on one serves traffic, reports healthy and does no "+
						"real work. Comment the line out so the choice is explicit.",
					artifact, name)
			}
		})
	}
}

// TestProductionComposeDefaultsToTheRealExecutionTransport verifies the
// production compose file selects the broker-backed transport by default rather
// than inheriting the offline development default. The application refuses a
// stub in production, so a compose file that passed the stub through would make
// every production deployment fail to start for a reason the operator has to
// diagnose from application logs instead of from the file they deployed.
func TestProductionComposeDefaultsToTheRealExecutionTransport(t *testing.T) {
	body := readRepoFile(t, "../docker-compose.production.yml")
	require.Contains(t, body, `AUSTRO_EXECUTION_TRANSPORT_BACKEND: "${AUSTRO_EXECUTION_TRANSPORT_BACKEND:-rabbitmq}"`,
		"the production compose file must default the execution transport to rabbitmq, not to the development stub")
}

// TestGeneralExampleDoesNotClaimProductionWhileShippingStubs is the other half
// of the rule above, and the reason .env.example is allowed to keep its stubs.
//
// The general example ships AUSTRO_AI_BACKEND=stub and AUSTRO_PUBLISH_BACKEND=stub
// on purpose: that is what makes a fresh clone run with no account, no
// credential and no egress, and it is the property this project is built
// around. Forcing a real provider on every new user to satisfy a production
// rule would be a worse outcome than the one it prevents.
//
// Those stubs are safe for exactly one reason — the file does not declare itself
// production, and the application refuses a stub the moment AUSTRO_ENV is
// production. So the invariant worth enforcing is not "no stubs" but "stubs and
// a production claim never coexist in the same file".
func TestGeneralExampleDoesNotClaimProductionWhileShippingStubs(t *testing.T) {
	body := readRepoFile(t, "../.env.example")

	for _, line := range activeAssignments(t, body) {
		name, value, ok := assignment(line)
		if !ok {
			continue
		}
		require.NotEqualf(t, "production", strings.TrimSpace(value),
			".env.example ships %s=%s while also shipping stub adapters. The stubs are "+
				"safe only while the file does not claim production; claiming it here would "+
				"make the example a configuration that refuses to start.", name, value)
	}

	// Sanity-check that the file really does ship the offline defaults, so this
	// test cannot pass by the stubs being removed.
	require.Contains(t, body, "AUSTRO_AI_BACKEND=stub",
		"the general example should still ship the offline AI default")
	require.Contains(t, body, "AUSTRO_EXECUTION_TRANSPORT_BACKEND=stub",
		"the general example should still ship the offline execution transport default")
}

// TestOfflineDevelopmentComposeIsUnchangedByTheReleaseGate verifies the release
// gate did not leak into the development stack. `docker compose up` on a fresh
// clone with no configuration must still start on the offline defaults; that is
// the property that makes the project runnable by anyone, and a release gate that
// broke it would have been fixed by removing the gate.
func TestOfflineDevelopmentComposeIsUnchangedByTheReleaseGate(t *testing.T) {
	body := readRepoFile(t, "../docker-compose.yml")
	require.NotContains(t, body, "AUSTRO_EXECUTION_TRANSPORT_BACKEND",
		"the development compose file must not pin the execution transport: the stub default is the point")
	require.NotContains(t, body, "AUSTRO_EXECUTION_WORKSPACE_IDS",
		"the development compose file must not pin execution workspaces")
	require.NotContains(t, body, "AUSTRO_ALLOW_STUB_BACKENDS",
		"the development compose file never needs the production stub exemption")
}

// readRepoFile reads a repository-relative path from a test, failing the test
// with the resolved path when the file is missing, so a moved artifact reports
// itself instead of looking like a content failure.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	require.NoError(t, err)
	body, err := os.ReadFile(abs)
	require.NoErrorf(t, err, "cannot read %s (resolved to %s)", rel, abs)
	return string(body)
}

// activeAssignments returns the non-comment, non-blank lines of an env file.
func activeAssignments(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// assignment splits "NAME=value" and reports whether the line is one.
func assignment(line string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(name), value, true
}
