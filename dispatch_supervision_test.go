package main

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"austro-os/internal/config"
)

// TestShutdownSignalsIncludeSIGHUP exists because of a specific, documented
// platform behaviour: AlwaysData restarts a background Service by sending
// SIGHUP to its foreground process
// (https://help.alwaysdata.com/en/docs/web-hosting/services).
//
// SIGHUP is not one of the signals the Go runtime handles by default, so
// omitting it from this set means the default action applies and the process
// terminates in place: no request drain, no deferred close of the database,
// Redis or broker connections, and no log line saying why. The system survives
// that — dispatch and consumption are fenced and at-least-once — but it should
// be a choice made once, not a silent cost paid on every platform restart.
func TestShutdownSignalsIncludeSIGHUP(t *testing.T) {
	sigs := shutdownSignals()
	require.Contains(t, sigs, os.Signal(syscall.SIGHUP),
		"a platform restart sends SIGHUP; it must begin a graceful shutdown")
	require.Contains(t, sigs, os.Signal(os.Interrupt))
	require.Contains(t, sigs, os.Signal(syscall.SIGTERM))
}

// TestDispatchModeHelpersAgreeWithTheValidator checks the three decisions the
// entrypoint makes from the setting, across every value the validator accepts
// plus one it must not.
func TestDispatchModeHelpersAgreeWithTheValidator(t *testing.T) {
	cases := []struct {
		role        string
		mode        string
		sweeps      bool
		serviceOnly bool
	}{
		{config.ProcessRoleAPI, "api", true, false},
		{config.ProcessRoleAPI, "API", true, false},
		{config.ProcessRoleAPI, "  api  ", true, false},
		{config.ProcessRoleDispatcher, "service", true, true},
		{config.ProcessRoleDispatcher, "Service", true, true},
		{config.ProcessRoleAPI, "off", false, false},
		{config.ProcessRoleWorker, "off", false, false},
		// The loader always fills the role, so an empty one is a hand-built
		// Config rather than a deployment, and it means the historical default:
		// the API process, which also sweeps. A blanked-out deployment value is
		// rejected by the validator instead of landing here.
		{"", "api", true, false},
		{"", "off", false, false},
		// Case and padding are tolerated by the validator, so the entrypoint
		// has to tolerate them too: a process that declared "WORKER" is a
		// worker, and one that declared "  Dispatcher  " is the dispatcher.
		{"  Worker  ", "off", false, false},
		{"DISPATCHER", "service", true, true},
		// Values the validator rejects. The process must not act on them as
		// though they meant something valid: LoadStrict refuses to start, so
		// this branch is unreachable, and a helper that guessed "api" here
		// would make a tightened rule ineffective.
		{config.ProcessRoleAPI, "nonsense", false, false},
		{"nonsense", "off", false, false},
		// The role is the statement about which process this is, so it decides
		// the branch even when the mode contradicts it. Validate refuses this
		// pairing before startup; the entrypoint must not silently become the
		// dispatcher because the mode alone happened to match.
		{config.ProcessRoleAPI, "service", true, false},
		{config.ProcessRoleWorker, "service", true, false},
	}
	for _, tc := range cases {
		name := tc.role + "/" + tc.mode
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{ProcessRole: tc.role, ExecutionDispatchMode: tc.mode}
			require.Equal(t, tc.sweeps, sweepsHere(cfg), "sweepsHere(%q)", name)
			require.Equal(t, tc.serviceOnly, isDispatcherRole(cfg), "isDispatcherRole(%q)", name)
		})
	}
}

// TestDispatchServiceOnlyAndSweepingAreNotMutuallyExclusive records the one
// combination that is easy to get wrong: a service-mode process both sweeps and
// is the standalone dispatcher. It is a switch on how the process is
// supervised, not on whether it dispatches.
func TestDispatchServiceOnlyAndSweepingAreNotMutuallyExclusive(t *testing.T) {
	cfg := &config.Config{
		ProcessRole:           config.ProcessRoleDispatcher,
		ExecutionDispatchMode: config.ExecutionDispatchModeService,
	}
	require.True(t, sweepsHere(cfg))
	require.True(t, isDispatcherRole(cfg))
}

// TestServiceModeProcessHoldsNoListenerAndNoRedis records the property the
// split deployment depends on: the dispatcher needs no port and no Redis.
//
// Both facts are the reason a hosting platform with an unfiltered public port
// range can host the dispatcher at all. A port it does not open cannot be
// reached and so needs no network rule; a datastore it does not dial cannot be
// its single point of failure, and demanding the credential would mean handing
// the execution queue's blast radius the whole memory store.
func TestServiceModeProcessHoldsNoListenerAndNoRedis(t *testing.T) {
	cfg := &config.Config{
		ProcessRole:           config.ProcessRoleDispatcher,
		ExecutionDispatchMode: config.ExecutionDispatchModeService,
	}
	require.False(t, cfg.ServesHTTP(), "the dispatcher must never open a listener")
	require.False(t, cfg.UsesRedis(), "the dispatcher must never dial Redis")
	require.False(t, cfg.ConsumesExecutionQueue(), "the dispatcher is not the consumer")
	require.True(t, cfg.DispatchesHere())
}

// TestWorkerRoleHoldsNoListenerAndNoRedis is the same property for the
// consumer, whose entire surface is the broker connection.
func TestWorkerRoleHoldsNoListenerAndNoRedis(t *testing.T) {
	cfg := &config.Config{
		ProcessRole:           config.ProcessRoleWorker,
		ExecutionDispatchMode: config.ExecutionDispatchModeOff,
	}
	require.False(t, cfg.ServesHTTP(), "the worker must never open a listener")
	require.False(t, cfg.UsesRedis(), "the worker must never dial Redis")
	require.True(t, cfg.ConsumesExecutionQueue())
	require.False(t, cfg.DispatchesHere(), "two sweepers make ownership nominal")
}

// TestOnlyTheAPIRoleServesHTTPOrUsesRedis pins the inverse of the two tests
// above: whatever a role's sweep mode is, the API role is the only one that
// serves traffic or holds a Redis connection. This is the property that lets
// the dispatcher and the worker be deployed onto a host with neither.
func TestOnlyTheAPIRoleServesHTTPOrUsesRedis(t *testing.T) {
	for _, role := range []string{config.ProcessRoleAPI, config.ProcessRoleDispatcher, config.ProcessRoleWorker} {
		for _, mode := range []string{
			config.ExecutionDispatchModeAPI,
			config.ExecutionDispatchModeService,
			config.ExecutionDispatchModeOff,
		} {
			cfg := &config.Config{ProcessRole: role, ExecutionDispatchMode: mode}
			if role == config.ProcessRoleAPI {
				require.True(t, cfg.ServesHTTP(), "role %q must serve HTTP", role)
				require.True(t, cfg.UsesRedis(), "role %q must use Redis", role)
			} else {
				require.False(t, cfg.ServesHTTP(), "role %q with mode %q must not serve HTTP", role, mode)
				require.False(t, cfg.UsesRedis(), "role %q with mode %q must not use Redis", role, mode)
			}
		}
	}
}

// TestAPIBinaryRefusesTheWorkerRole is the mirror of the worker binary's
// assertion, and it guards a specific misconfiguration.
//
// role=worker with mode=off is a valid configuration, so Validate accepts it. If
// the API binary did not refuse it, it would fall through the dispatcher branch
// and open an HTTP listener for a process that was explicitly declared to hold
// none — and the declared role would be the only evidence that anything went
// wrong, in a file nobody reads during an incident.
func TestAPIBinaryRefusesTheWorkerRole(t *testing.T) {
	require.NoError(t, assertServableRole(&config.Config{ProcessRole: config.ProcessRoleAPI}))
	require.NoError(t, assertServableRole(&config.Config{ProcessRole: config.ProcessRoleDispatcher}),
		"the dispatcher shares this binary, which is the point")

	err := assertServableRole(&config.Config{ProcessRole: config.ProcessRoleWorker})
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_PROCESS_ROLE",
		"the error must name the setting to fix")
}

// TestAPIBinaryRefusesTheWorkerRoleHoweverItIsSpelled is the regression test
// for the exact bypass this assertion is here to close.
//
// The validator lowercases and trims the configured role, so "WORKER" and
// "  worker  " are accepted as the worker role. An entrypoint that compared the
// raw field against the constant "worker" would not recognise them, would treat
// the process as the API, and would open an HTTP listener for a deployment that
// declared itself to hold no port. The gap between what Validate accepts and
// what the entrypoint compares is the whole bug, so the test spans both
// spellings rather than only the canonical one.
func TestAPIBinaryRefusesTheWorkerRoleHoweverItIsSpelled(t *testing.T) {
	for _, role := range []string{config.ProcessRoleWorker, "WORKER", "  worker  ", "\tWorker\n"} {
		cfg := &config.Config{
			ProcessRole:           role,
			ExecutionDispatchMode: config.ExecutionDispatchModeOff,
		}
		require.Error(t, assertServableRole(cfg),
			"role %q is the worker role and this binary must not serve it", role)
		require.False(t, isDispatcherRole(cfg),
			"role %q must not branch into the dispatcher either", role)
	}

	// The same tolerance applies in the other direction: a dispatcher declared
	// in any spelling is still the dispatcher, and a differently-cased api
	// role is still servable.
	for _, role := range []string{config.ProcessRoleDispatcher, "DISPATCHER", " dispatcher "} {
		cfg := &config.Config{
			ProcessRole:           role,
			ExecutionDispatchMode: config.ExecutionDispatchModeService,
		}
		require.NoError(t, assertServableRole(cfg), "role %q shares this binary", role)
		require.True(t, isDispatcherRole(cfg), "role %q is the dispatcher", role)
	}
}

// TestUnknownExecutionTransportIsRefusedNotStubbed is the no-silent-fallback
// check at the point where the transport is actually built.
//
// A dispatcher pointed at an unrecognised transport has two possible outcomes:
// fail loudly, or quietly log every dispatch and report success without
// contacting a broker. The second is the offline stub wearing the costume of a
// working pipeline, so the factory refuses instead of defaulting.
func TestUnknownExecutionTransportIsRefusedNotStubbed(t *testing.T) {
	for _, backend := range []string{"amqp", "kafka", "RABBITMQ", "in-memory", "true"} {
		t.Run(backend, func(t *testing.T) {
			cfg := &config.Config{
				ExecutionTransportBackend: backend,
				ExecutionQueue:            config.DefaultExecutionQueue,
			}
			pub, cleanup, err := newExecutionPublisher(cfg)
			require.Error(t, err, "backend %q must be refused", backend)
			require.Nil(t, pub)
			require.Nil(t, cleanup)
		})
	}
}

// TestStubTransportPublishesDeterministically covers the other end: the stub is
// still reachable, but only when it is asked for by name or by leaving the
// setting empty, and the returned cleanup is always callable so a caller
// cannot leak a sink by branching on it.
func TestStubTransportPublishesDeterministically(t *testing.T) {
	for _, backend := range []string{"", config.ExecutionTransportBackendStub} {
		t.Run("backend="+backend, func(t *testing.T) {
			cfg := &config.Config{
				ExecutionTransportBackend: backend,
				ExecutionQueue:            config.DefaultExecutionQueue,
			}
			pub, cleanup, err := newExecutionPublisher(cfg)
			require.NoError(t, err)
			require.NotNil(t, pub)
			require.NotNil(t, cleanup)
			cleanup() // must not panic
		})
	}
}
