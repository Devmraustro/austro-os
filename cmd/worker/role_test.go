package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"austro-os/internal/config"
)

// TestWorkerBinaryRefusesEveryOtherRole documents the entrypoint assertion.
//
// The risk it guards is a shared environment file. On a hosting platform where
// one file can feed several units, a single AUSTRO_PROCESS_ROLE is the most
// likely way this process gets misconfigured, and the two possible repairs are
// very different. Refusing names the file to fix; silently overriding would leave
// the API's role in that file looking deliberate while the worker ran under a
// different one, so the next person to read the configuration would be misled
// about what the deployment actually does.
func TestWorkerBinaryRefusesEveryOtherRole(t *testing.T) {
	require.NoError(t, assertWorkerRole(&config.Config{ProcessRole: config.ProcessRoleWorker}),
		"the worker role is the only one this binary serves")

	for _, role := range []string{
		config.ProcessRoleAPI,
		config.ProcessRoleDispatcher,
		"API",
		"", // never configured: a hand-built Config, so the default applies
	} {
		t.Run(role, func(t *testing.T) {
			err := assertWorkerRole(&config.Config{ProcessRole: role})
			require.Error(t, err, "role %q must be refused by the worker binary", role)
			require.Contains(t, err.Error(), "AUSTRO_PROCESS_ROLE",
				"the error must name the setting to fix, not just the fact of failure")
		})
	}
}

// TestWorkerBinaryAcceptsTheWorkerRoleInAnySpelling closes the mirror image of
// the API binary's check.
//
// The validator accepts the worker role in any case and with surrounding
// padding, so a deployment that wrote AUSTRO_PROCESS_ROLE="WORKER" passes
// validation and then reaches the entrypoint. An entrypoint comparing the raw
// field against the lowercase constant would refuse that deployment for a
// reason that has nothing to do with what it declared, and the operator's only
// visible symptom would be a Service that will not start. The assertion has to
// read the role the same way the validator did.
func TestWorkerBinaryAcceptsTheWorkerRoleInAnySpelling(t *testing.T) {
	for _, role := range []string{config.ProcessRoleWorker, "WORKER", "  worker  ", "Worker"} {
		t.Run(role, func(t *testing.T) {
			require.NoError(t, assertWorkerRole(&config.Config{ProcessRole: role}),
				"role %q is the worker role and this binary serves it", role)
		})
	}
}

// TestWorkerConfigurationValidatesForItsRole is the environment-level half of the
// same contract: the exact configuration a worker deployment needs must pass
// validation on its own, with no workspace list, no companion declaration, and no
// Redis address.
//
// Those three absences are the point. The worker holds no workspaces because it
// never sweeps; it declares no companion because a consumer declining to dispatch
// is the definition of a consumer rather than a gap; and it names no Redis
// because it never dials one. Demanding any of them would force a meaningless
// setting on a process whose entire surface is one broker connection, and a
// setting nobody reads is exactly the kind of configuration that later gets
// "fixed" by someone who assumes it is load-bearing.
func TestWorkerConfigurationValidatesForItsRole(t *testing.T) {
	cfg := &config.Config{
		ProcessRole:               config.ProcessRoleWorker,
		ExecutionDispatchMode:     config.ExecutionDispatchModeOff,
		ExecutionWorkspaceIDs:     nil,
		ExecutionTransportBackend: config.ExecutionTransportBackendRabbitMQ,
		Environment:               config.EnvironmentProduction,
		HeartbeatInterval:         15 * time.Second,
		HeartbeatStaleAfter:       60 * time.Second,
		PostgresDSN:               "postgres://austro:austro@db:5432/austro?sslmode=disable",
		RabbitMQURL:               "amqp://austro:austro@rabbitmq:5672",
		JWTSecret:                 "prod-access-secret-1234567890-abcdef",
		JWTRefreshSecret:          "prod-refresh-secret-1234567890-abcdef",
		AIBackend:                 config.AIBackendOpenAICompatible,
		AIModel:                   "austro-classifier",
		AIBaseURL:                 "https://ai.austro.internal/v1",
		AIAPIKey:                  "sk-prod-9f8e7d6c5b4a",
		PublishBackend:            config.PublishBackendGenericHTTP,
		PublishWebhookURL:         "https://hook.austro.internal/deliver",
		PublishToken:              "tok-prod-1a2b3c4d5e6f",

		// The execution lease settings are genuinely required here, and the
		// reason is worth recording: the worker is not a passive consumer. It
		// renews the claim it holds while it runs, and the sweep interval and
		// backoff bounds are the pacing the dispatcher and the worker's own
		// settlement path share. So this process does need the execution
		// configuration — just not the workspace list or the dispatch loop that
		// goes with it.
		ExecutionLeaseTTL:          60 * time.Second,
		ExecutionHeartbeatInterval: 20 * time.Second,
		ExecutionWindow:            50 * time.Second,
		ExecutionSweepInterval:     5 * time.Second,
		ExecutionRetryBackoffBase:  2 * time.Second,
		ExecutionRetryBackoffMax:   2 * time.Minute,
		ExecutionQueue:             config.DefaultExecutionQueue,
	}

	require.NoError(t, cfg.Validate(), "a worker must not need a datastore it never opens")
}
