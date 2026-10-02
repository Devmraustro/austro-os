package config

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A production configuration on a broker-backed transport, with the workspaces
// a real dispatching deployment would name. Every role and dispatch-mode rule is
// exercised against this shape, because the rules that matter only bite when
// production and the broker are both in play: on the offline defaults the
// dispatcher is a no-op anyway, so a broken rule there would be invisible.
func productionDispatchCore() *Config {
	cfg := secureCore()
	cfg.Environment = EnvironmentProduction
	cfg.AIBackend = AIBackendOpenAICompatible
	cfg.AIModel = "austro-classifier"
	cfg.AIBaseURL = "https://ai.austro.internal/v1"
	cfg.AIAPIKey = "sk-prod-9f8e7d6c5b4a"
	cfg.PublishBackend = PublishBackendGenericHTTP
	cfg.PublishWebhookURL = "https://hook.austro.internal/deliver"
	cfg.PublishToken = "tok-prod-1a2b3c4d5e6f"
	cfg.ExecutionTransportBackend = ExecutionTransportBackendRabbitMQ
	cfg.ExecutionQueue = DefaultExecutionQueue
	cfg.ExecutionWorkspaceIDs = []uuid.UUID{uuid.New()}
	cfg.ProcessRole = ProcessRoleAPI
	cfg.ExecutionDispatchMode = ExecutionDispatchModeAPI
	return cfg
}

// TestProductionRoleAndModeMatrix is the whole contract in one table.
//
// Every combination of role and mode is either accepted or refused, and the
// reason each refusal exists is the point of the test:
//
//   - api + off is the split deployment's API: HTTP only, sweep delegated. It
//     must be *possible*, because refusing it is what made the intended hosting
//     topology unreachable, and impossible for a different reason — see
//     TestProductionRefusesUndeclaredCompanionDispatcher.
//   - api + service is refused because it would serve HTTP from the process
//     whose entire purpose is to hold no port.
//   - dispatcher + api is refused because it would do the same thing from the
//     other direction.
//   - worker + anything that sweeps is refused: two sweepers make ownership a
//     fiction, and a worker with a listener exposes a port nothing needs.
func TestProductionRoleAndModeMatrix(t *testing.T) {
	cases := []struct {
		role     string
		mode     string
		accept   bool
		offender string
	}{
		// The single-process deployment: one binary, HTTP and the sweep.
		{ProcessRoleAPI, ExecutionDispatchModeAPI, true, ""},
		// The split deployment's API. Valid only with a declared companion,
		// which this case supplies below; the bare form is a separate test.
		{ProcessRoleAPI, ExecutionDispatchModeOff, true, ""},
		{ProcessRoleAPI, ExecutionDispatchModeService, false, "AUSTRO_EXECUTION_DISPATCH_MODE"},
		{ProcessRoleDispatcher, ExecutionDispatchModeService, true, ""},
		{ProcessRoleDispatcher, ExecutionDispatchModeAPI, false, "AUSTRO_EXECUTION_DISPATCH_MODE"},
		{ProcessRoleDispatcher, ExecutionDispatchModeOff, false, "AUSTRO_EXECUTION_DISPATCH_MODE"},
		{ProcessRoleWorker, ExecutionDispatchModeOff, true, ""},
		{ProcessRoleWorker, ExecutionDispatchModeAPI, false, "AUSTRO_EXECUTION_DISPATCH_MODE"},
		{ProcessRoleWorker, ExecutionDispatchModeService, false, "AUSTRO_EXECUTION_DISPATCH_MODE"},
	}

	for _, tc := range cases {
		t.Run(tc.role+"_"+tc.mode, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg.ProcessRole = tc.role
			cfg.ExecutionDispatchMode = tc.mode
			// The workspace list belongs to whichever process sweeps, and the
			// "off" rules reject it otherwise.
			if tc.mode == ExecutionDispatchModeOff {
				cfg.ExecutionWorkspaceIDs = nil
			}
			if tc.role == ProcessRoleAPI && tc.mode == ExecutionDispatchModeOff {
				cfg.expectCompanionProcessesRaw = ProcessRoleDispatcher
			}
			if tc.role == ProcessRoleWorker {
				// The worker is not a dispatcher, so it holds no workspace list.
				cfg.ExecutionWorkspaceIDs = nil
			}

			err := cfg.Validate()
			if tc.accept {
				require.NoError(t, err)
				return
			}
			require.Error(t, err, "role %q with mode %q must be refused", tc.role, tc.mode)
			require.Contains(t, err.Error(), tc.offender)
		})
	}
}

// TestProductionRefusesUndeclaredCompanionDispatcher is the rule that closes the
// silent-stop gap without making the split topology impossible.
//
// The failure it prevents is specific and total: with no process sweeping,
// nothing ever publishes to the execution queue, executions accumulate in "due"
// state in PostgreSQL, the worker sits idle with an empty queue, and the API
// keeps answering 200 on /health/ready. Nothing in the system reports it.
//
// What the rule does *not* do is guess. A process cannot observe a companion on
// another host, so the honest options are "refuse to start" or "assume it is
// fine" — and the second is the bug. Requiring the deployment to name the
// dispatcher moves the claim from silently absent to explicitly stated, and the
// readiness endpoint then reports that dispatcher's actual liveness.
func TestProductionRefusesUndeclaredCompanionDispatcher(t *testing.T) {
	cfg := productionDispatchCore()
	cfg.ProcessRole = ProcessRoleAPI
	cfg.ExecutionDispatchMode = ExecutionDispatchModeOff
	cfg.ExecutionWorkspaceIDs = nil

	err := cfg.Validate()
	require.Error(t, err, "an API that does not sweep must name who does")
	require.Contains(t, err.Error(), "AUSTRO_EXPECT_COMPANION_PROCESSES")
}

// TestDeclaredCompanionMakesTheSplitTopologyValid is the other half of the rule
// above: naming the companion is sufficient to start, and nothing more. It
// does not make the process healthy, because it cannot — that is what the
// heartbeat check on /health/ready is for.
func TestDeclaredCompanionMakesTheSplitTopologyValid(t *testing.T) {
	cfg := productionDispatchCore()
	cfg.ProcessRole = ProcessRoleAPI
	cfg.ExecutionDispatchMode = ExecutionDispatchModeOff
	cfg.ExecutionWorkspaceIDs = nil
	cfg.expectCompanionProcessesRaw = "dispatcher"

	require.NoError(t, cfg.Validate())
	require.True(t, cfg.ExpectsCompanion(ProcessRoleDispatcher))
	require.False(t, cfg.DispatchesHere(), "the API must not sweep when it declared a dispatcher")
	require.True(t, cfg.ServesHTTP(), "the API is still the only process serving traffic")
}

// TestExpectsCompanionIsOnlyATopologyClaim pins the distinction the whole
// mechanism rests on. ExpectsCompanion answers "does the deployment say a
// dispatcher runs elsewhere", which is knowable from configuration alone. It
// must never be read as "a dispatcher is running", which is not knowable from
// inside this process — that is the observation, and it lives in the heartbeat
// table.
func TestExpectsCompanionIsOnlyATopologyClaim(t *testing.T) {
	cfg := productionDispatchCore()
	cfg.expectCompanionProcessesRaw = " Dispatcher , WORKER ,dispatcher"

	require.True(t, cfg.ExpectsCompanion(ProcessRoleDispatcher))
	require.True(t, cfg.ExpectsCompanion(ProcessRoleWorker),
		"tokens are case-folded and whitespace-trimmed like every other list here")
	require.Equal(t, []string{ProcessRoleDispatcher, ProcessRoleWorker}, cfg.DeclaredCompanions(),
		"duplicates collapse and configuration order is preserved")
	require.False(t, cfg.ExpectsCompanion(ProcessRoleAPI))
}

// TestCompanionListRejectsGarbage refuses to let a typo satisfy a requirement.
// A misspelled role is not a weaker declaration, it is a deployment whose
// dispatcher was never registered — the same silent gap the rule exists to
// close, reached by a different route.
func TestCompanionListRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"Dispatchr", "dispatcher,workre", "true", "1", "on", "yes"} {
		t.Run(raw, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg.ProcessRole = ProcessRoleAPI
			cfg.ExecutionDispatchMode = ExecutionDispatchModeOff
			cfg.ExecutionWorkspaceIDs = nil
			cfg.expectCompanionProcessesRaw = raw

			err := cfg.Validate()
			require.Error(t, err, "companion list %q must be refused", raw)
			require.Contains(t, err.Error(), "AUSTRO_EXPECT_COMPANION_PROCESSES")
		})
	}
}

// TestCompanionListRejectsDeclaringItself catches the self-referential case. A
// process that lists itself as the companion it is waiting for can never become
// ready, and the misconfiguration is invisible until someone reads the
// environment file.
func TestCompanionListRejectsDeclaringItself(t *testing.T) {
	cfg := productionDispatchCore()
	cfg.ProcessRole = ProcessRoleDispatcher
	cfg.ExecutionDispatchMode = ExecutionDispatchModeService
	cfg.expectCompanionProcessesRaw = ProcessRoleDispatcher

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_EXPECT_COMPANION_PROCESSES")
}

// TestUnknownProcessRoleIsRejectedNotCoerced makes a role typo fail closed. The
// zero value is the API, so an unrecognised role treated as "api" would open an
// HTTP listener on a process configured to be something else — the same
// fail-closed requirement the dispatch mode has, for the same reason.
func TestUnknownProcessRoleIsRejectedNotCoerced(t *testing.T) {
	for _, raw := range []string{"API-ONLY", "workre", "true", "1", "none", "  ", "\t"} {
		t.Run(raw, func(t *testing.T) {
			cfg := withProcessRole(productionDispatchCore(), raw)

			err := cfg.Validate()
			require.Error(t, err, "role %q must not be accepted", raw)
			require.Contains(t, err.Error(), "AUSTRO_PROCESS_ROLE")
		})
	}
}

// TestProcessRoleToleratesCaseAndPadding keeps the setting writable by hand
// without turning a formatting difference into an outage.
func TestProcessRoleToleratesCaseAndPadding(t *testing.T) {
	for _, tc := range []struct{ role, mode string }{
		{"  DISPATCHER  ", ExecutionDispatchModeService},
		{"Dispatcher", ExecutionDispatchModeService},
		{"WORKER", ExecutionDispatchModeOff},
		{"worker  ", ExecutionDispatchModeOff},
	} {
		t.Run(tc.role, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg = withProcessRole(cfg, tc.role)
			cfg.ExecutionDispatchMode = tc.mode
			if tc.mode == ExecutionDispatchModeOff {
				cfg.ExecutionWorkspaceIDs = nil
			}
			require.NoError(t, cfg.Validate())
		})
	}
}

// TestProductionRefusesOffWithWorkspacesConfigured covers the mirror image: a
// process that is handed the workspace list of a dispatcher and then told not
// to dispatch. It looks like a dispatcher in the environment file and behaves
// as a bystander, which is worse than either configuration alone.
func TestProductionRefusesOffWithWorkspacesConfigured(t *testing.T) {
	cfg := productionDispatchCore()
	cfg.ExecutionDispatchMode = ExecutionDispatchModeOff
	cfg.ExecutionWorkspaceIDs = []uuid.UUID{uuid.New(), uuid.New()}
	cfg.expectCompanionProcessesRaw = ProcessRoleDispatcher

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_WORKSPACE_IDS")
}

// TestProductionAcceptsEitherSingleDispatcher proves the guard above did not
// simply forbid the standalone dispatcher. Exactly one process may sweep, and
// these are the two supported ways of saying which, each with the role that
// makes it coherent.
func TestProductionAcceptsEitherSingleDispatcher(t *testing.T) {
	for _, tc := range []struct{ role, mode string }{
		{ProcessRoleAPI, ExecutionDispatchModeAPI},
		{ProcessRoleDispatcher, ExecutionDispatchModeService},
	} {
		t.Run(tc.role+"_"+tc.mode, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg.ProcessRole = tc.role
			cfg.ExecutionDispatchMode = tc.mode
			require.NoError(t, cfg.Validate())
		})
	}
}

// TestDispatcherServiceModeStillRequiresWorkspaces pins the consequence of
// moving the sweep loop to its own process: that process has nothing else to
// do, so a service started with no workspaces would sit supervised and idle
// forever, looking exactly like a working dispatcher.
func TestDispatcherServiceModeStillRequiresWorkspaces(t *testing.T) {
	cfg := productionDispatchCore()
	cfg.ProcessRole = ProcessRoleDispatcher
	cfg.ExecutionDispatchMode = ExecutionDispatchModeService
	cfg.ExecutionWorkspaceIDs = nil

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_WORKSPACE_IDS")
}

// withDispatchMode sets the mode the way the loader does — parsed field and raw
// environment text together — so a test case exercises the path a real
// deployment takes rather than a hand-built literal that the loader could never
// produce.
func withDispatchMode(cfg *Config, raw string) *Config {
	cfg.executionDispatchModeRaw = raw
	cfg.ExecutionDispatchMode = getEnv("AUSTRO_EXECUTION_DISPATCH_MODE", ExecutionDispatchModeAPI)
	if raw != "" {
		cfg.ExecutionDispatchMode = raw
	}
	return cfg
}

// withProcessRole does the same for the role. It exists for the same reason:
// the distinction between "nobody configured a role" and "somebody configured
// whitespace" is only visible in the raw text, so a test that sets the parsed
// field alone is not describing a case the loader can produce.
func withProcessRole(cfg *Config, raw string) *Config {
	cfg.processRoleRaw = raw
	cfg.ProcessRole = getEnv("AUSTRO_PROCESS_ROLE", ProcessRoleAPI)
	if raw != "" {
		cfg.ProcessRole = raw
	}
	return cfg
}

// TestUnknownDispatchModeIsRejectedNotCoerced makes sure a typo fails closed.
// Treating an unrecognised mode as "api" would restart the sweep loop in the
// HTTP process — the exact placement this setting exists to move away from —
// while reporting a successful startup.
func TestUnknownDispatchModeIsRejectedNotCoerced(t *testing.T) {
	for _, raw := range []string{"API-ONLY", "servce", "disabled", "true", "1", "none", "  ", "off but with a comment"} {
		t.Run(raw, func(t *testing.T) {
			cfg := withDispatchMode(productionDispatchCore(), raw)

			err := cfg.Validate()
			require.Error(t, err, "mode %q must not be accepted", raw)
			require.Contains(t, err.Error(), "AUSTRO_EXECUTION_DISPATCH_MODE")
		})
	}
}

// TestDispatchModeToleratesCaseAndPadding keeps the setting writable by hand
// without turning a formatting difference into an outage. Case and surrounding
// whitespace are normalised; the value itself is still checked exactly.
func TestDispatchModeToleratesCaseAndPadding(t *testing.T) {
	for _, raw := range []string{"  SERVICE  ", "Service"} {
		t.Run(raw, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg.ProcessRole = ProcessRoleDispatcher
			cfg.ExecutionDispatchMode = raw
			require.NoError(t, cfg.Validate())
		})
	}
	for _, raw := range []string{"API", "api  "} {
		t.Run(raw, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg.ExecutionDispatchMode = raw
			require.NoError(t, cfg.Validate())
		})
	}
}

// TestPaddedOffIsStillOff proves the normalisation is applied by the production
// guard and not only by the enum check. If " off " were compared literally, it
// would fail the enum test and be reported as an unknown mode — which reads
// like a typo and sends the operator looking for a misspelling, when what they
// wrote was a perfectly clear "off" with a stray space that happened to
// disable the safety rule it names.
func TestPaddedOffIsStillOff(t *testing.T) {
	cfg := productionDispatchCore()
	cfg.ExecutionDispatchMode = " Off "
	cfg.ExecutionWorkspaceIDs = []uuid.UUID{uuid.New()}
	cfg.expectCompanionProcessesRaw = ProcessRoleDispatcher

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUSTRO_EXECUTION_WORKSPACE_IDS",
		"a padded 'off' must reach the same production rules as an exact one")
}

// TestDevelopmentMayParkTheDispatcher is the counterpart: running the API
// locally with admission working and dispatch deliberately parked is a normal
// way to develop, and a production-only rule must not break it.
func TestDevelopmentMayParkTheDispatcher(t *testing.T) {
	cfg := secureCore()
	cfg.ExecutionTransportBackend = ExecutionTransportBackendStub
	cfg.ExecutionDispatchMode = ExecutionDispatchModeOff
	require.NoError(t, cfg.Validate())
}

// TestProcessRoleDefaultsToAPIInTheAPIProcess is the compatibility guarantee.
// Every deployment built before this setting existed runs one binary that serves
// HTTP and sweeps, so the default has to keep meaning exactly that.
func TestProcessRoleDefaultsToAPIInTheAPIProcess(t *testing.T) {
	t.Setenv("AUSTRO_PROCESS_ROLE", "")
	t.Setenv("AUSTRO_EXECUTION_DISPATCH_MODE", "")
	cfg := Load()
	require.Equal(t, ProcessRoleAPI, cfg.ProcessRole)
	require.True(t, cfg.ServesHTTP())
	require.True(t, cfg.UsesRedis())
	require.Equal(t, ExecutionDispatchModeAPI, cfg.ExecutionDispatchMode)
	require.True(t, sweepsHereForTest(cfg))
}

// TestProcessRoleAndDispatchModeAreReadFromTheEnvironment proves the wiring, so
// a deployment that sets the variables actually gets the process it described
// rather than silently keeping the old single-binary behaviour.
func TestProcessRoleAndDispatchModeAreReadFromTheEnvironment(t *testing.T) {
	t.Setenv("AUSTRO_PROCESS_ROLE", "dispatcher")
	t.Setenv("AUSTRO_EXECUTION_DISPATCH_MODE", "service")
	cfg := Load()
	require.Equal(t, ProcessRoleDispatcher, cfg.ProcessRole)
	require.False(t, cfg.ServesHTTP())
	require.False(t, cfg.UsesRedis())
	require.True(t, dispatchServiceOnlyForTest(cfg))
	require.True(t, sweepsHereForTest(cfg))

	t.Setenv("AUSTRO_PROCESS_ROLE", "api")
	t.Setenv("AUSTRO_EXECUTION_DISPATCH_MODE", "off")
	t.Setenv("AUSTRO_EXPECT_COMPANION_PROCESSES", "dispatcher")
	cfg = Load()
	require.False(t, sweepsHereForTest(cfg))
	require.True(t, cfg.ExpectsCompanion(ProcessRoleDispatcher))
	require.Equal(t, []string{ProcessRoleDispatcher}, cfg.DeclaredCompanions())
}

// TestRedisIsRequiredOnlyByTheRolesThatDialIt is the least-privilege
// consequence of the split. The dispatcher and the worker never open a Redis
// connection, so demanding the credential from them would mean the memory
// store's availability is a startup dependency of the execution queue — a
// dependency that does not exist in the code and would be invisible to anyone
// reading it.
func TestRedisIsRequiredOnlyByTheRolesThatDialIt(t *testing.T) {
	for _, tc := range []struct {
		role   string
		usable bool
	}{
		{ProcessRoleAPI, true},
		{ProcessRoleDispatcher, false},
		{ProcessRoleWorker, false},
	} {
		t.Run(tc.role, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg.ProcessRole = tc.role
			cfg.ExecutionDispatchMode = ExecutionDispatchModeService
			if tc.role != ProcessRoleDispatcher {
				cfg.ExecutionDispatchMode = ExecutionDispatchModeOff
			}
			if tc.role != ProcessRoleDispatcher {
				cfg.ExecutionWorkspaceIDs = nil
			}
			if tc.role == ProcessRoleAPI {
				cfg.expectCompanionProcessesRaw = ProcessRoleDispatcher
			}
			cfg.RedisAddr = ""

			err := cfg.Validate()
			if tc.usable {
				require.Error(t, err, "the API dials Redis, so it must be configured")
				require.Contains(t, err.Error(), "AUSTRO_REDIS_ADDR")
			} else {
				require.NoError(t, err, "role %q never dials Redis, so it must not require it", tc.role)
			}
		})
	}
}

// TestStalenessWindowMustOutliveTheBeatPeriod guards the one setting relationship
// that can turn this mechanism into noise. A window at or below the beat period
// lets a reader check between two writes and call a running companion dead; a
// readiness endpoint that cries wolf is one operators learn to ignore, which
// loses the alarm the heartbeat exists to raise.
func TestStalenessWindowMustOutliveTheBeatPeriod(t *testing.T) {
	for _, tc := range []struct {
		name          string
		interval      time.Duration
		stale         time.Duration
		expectErrorAt string
	}{
		{"window shorter than the period", 30 * time.Second, 10 * time.Second, "AUSTRO_HEARTBEAT_STALE_AFTER"},
		{"window equal to the period", 30 * time.Second, 30 * time.Second, "AUSTRO_HEARTBEAT_STALE_AFTER"},
		{"zero period", 0, 60 * time.Second, "AUSTRO_HEARTBEAT_INTERVAL"},
		{"zero window", 15 * time.Second, 0, "AUSTRO_HEARTBEAT_STALE_AFTER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg.HeartbeatInterval = tc.interval
			cfg.HeartbeatStaleAfter = tc.stale

			err := cfg.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectErrorAt)
		})
	}
}

// TestHeartbeatDurationsMustParse keeps a typo from silently falling back to
// the default. A window that quietly became 60s because someone wrote "60" is a
// window nobody chose.
func TestHeartbeatDurationsMustParse(t *testing.T) {
	for _, raw := range []string{"60", "1m30", "sixty seconds", "-"} {
		t.Run(raw, func(t *testing.T) {
			cfg := productionDispatchCore()
			cfg.heartbeatStaleAfterRaw = raw
			cfg.HeartbeatStaleAfter = envDuration("AUSTRO_HEARTBEAT_STALE_AFTER", 60*time.Second)

			err := cfg.Validate()
			require.Error(t, err, "an unparseable duration must not be accepted")
			require.Contains(t, err.Error(), "AUSTRO_HEARTBEAT_STALE_AFTER")
		})
	}
}

// sweepsHereForTest and dispatchServiceOnlyForTest restate the entrypoint's
// decision from the same normalisation the validator uses, so a rule that
// accepts a value the process cannot act on is caught here.
func sweepsHereForTest(c *Config) bool {
	switch c.ExecutionDispatchMode {
	case ExecutionDispatchModeAPI, ExecutionDispatchModeService:
		return true
	default:
		return false
	}
}

func dispatchServiceOnlyForTest(c *Config) bool {
	return c.ExecutionDispatchMode == ExecutionDispatchModeService
}
