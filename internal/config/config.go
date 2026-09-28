package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	logger "austro-os/internal/log"

	"github.com/google/uuid"
)

// Reserved adapter names. The default (stub) adapters are the only ones that
// may run without external configuration: they never contact a real external
// service and hold no credential. Selecting any other backend requires the
// corresponding external settings to be supplied securely (see Validate).
const (
	// AIBackendStub is the deterministic, offline AI adapter (default).
	AIBackendStub = "stub"
	// AIBackendLocal selects a LOCAL/free OpenAI-compatible endpoint (for
	// example a locally hosted model server or an in-house inference box).
	// Unlike openai-compatible, it does not require an API key: local
	// endpoints often need no credentials. Model and BaseURL are still
	// CONFIGURATION_REQUIRED. No block: the project must run without any paid
	// provider, and with no key where the operator's endpoint needs none.
	AIBackendLocal = "local"
	// AIBackendOpenAICompatible is a real OpenAI-compatible HTTP endpoint.
	// An API key is CONFIGURATION_REQUIRED; providers offering a free tier are
	// configured here later by the operator (never hardcoded).
	AIBackendOpenAICompatible = "openai-compatible"
	// PublishBackendStub is the deterministic, offline publishing adapter (default).
	PublishBackendStub = "stub"
	// PublishBackendGenericHTTP is a real generic HTTP delivery endpoint.
	PublishBackendGenericHTTP = "generic-http"
	// ExecutionTransportBackendStub is the deterministic, offline dispatch
	// transport (default): the dispatcher logs each fenced dispatch and reports
	// success without touching a broker. It is the AI execution analogue of the
	// AI/publishing stub adapters.
	ExecutionTransportBackendStub = "stub"
	// ExecutionTransportBackendRabbitMQ is the broker-backed dispatch transport:
	// each claimed retry is delivered durably to the dedicated execution queue
	// for the worker process to consume. It is wired by the entrypoint against
	// the configured RabbitMQ URL.
	ExecutionTransportBackendRabbitMQ = "rabbitmq"

	// EnvironmentProduction is the value of AUSTRO_ENV that enables the
	// production-only configuration rules. It gates rules that would break
	// offline development (notably: a production deployment may not run the
	// deterministic stub adapters), so the name is a contract with deployment
	// configuration rather than a free-form label.
	EnvironmentProduction = "production"

	// DefaultExecutionQueue is the default AUSTRO_EXECUTION_QUEUE, kept
	// identical to infrastructure/rabbitmq.ExecutionQueue. The literal is
	// repeated rather than imported so that the configuration layer never
	// depends on an infrastructure implementation (rabbitmq imports config, so
	// importing it here would be a cycle). The two are asserted equal by
	// TestDefaultExecutionQueueMatchesConfigDefault in the rabbitmq package,
	// which may import this one.
	DefaultExecutionQueue = "austro.executions"
)

type Config struct {
	ServerAddress string
	PostgresDSN   string

	// PostgresRuntimeDSN is the connection the application actually serves
	// traffic on. It must resolve to a role that is NOT a superuser, does NOT
	// have BYPASSRLS, and does NOT own the workspace-scoped tables, so the
	// row level security policies genuinely constrain it. PostgresDSN, by
	// contrast, is the schema owner used only to bootstrap and migrate.
	//
	// Leaving it empty derives a runtime DSN from PostgresDSN (same host,
	// database and password, role AUSTRO_POSTGRES_RUNTIME_USER or
	// "austro_app"), which is what keeps a developer running with no extra
	// configuration still on the unprivileged path: infrastructure/database
	// provisions the role and then verifies the live connection is
	// unprivileged before serving a single request. Deployments set it
	// explicitly so the runtime role has its own credential.
	PostgresRuntimeDSN string

	// PostgresAdminDSN is the explicit connection credential for the narrow
	// organization-level role (AUSTRO_POSTGRES_ADMIN_DSN). When empty, the
	// admin DSN is derived from the runtime DSN (same host and database, the
	// admin role name), which keeps the classic self-provisioned deployment
	// working unchanged. In pre-provisioned mode it is CONFIGURATION
	// REQUIRED: the platform created the role with its own credential, and
	// the application must connect with exactly that credential instead of a
	// derived one.
	PostgresAdminDSN string

	// PostgresPreProvisionedRoles (AUSTRO_POSTGRES_PREPROVISIONED_ROLES=true)
	// selects the externally-provisioned topology: the runtime and admin
	// roles already exist on the server, created by the platform or an
	// operator rather than by the application. The application then never
	// creates, alters or re-passwords them; at startup it verifies their
	// attributes against the live database (role exists, is a LOGIN role,
	// is not a superuser, holds no BYPASSRLS, holds neither CREATEROLE nor
	// CREATEDB) and fails fast on any deviation. Schema bootstrap through
	// the owner, the required GRANTs, RLS enablement and forcing, and the
	// live isolation canary run exactly as in self-provisioned mode.
	PostgresPreProvisionedRoles bool

	// preProvisionedRolesRaw is the raw environment text of
	// AUSTRO_POSTGRES_PREPROVISIONED_ROLES. Strict validation rejects a
	// value that does not parse as a boolean (fail-fast) instead of
	// silently treating a typo as "off".
	preProvisionedRolesRaw string

	RedisAddr        string
	RabbitMQURL      string
	RabbitMQQueue    string
	JWTSecret        string
	JWTRefreshSecret string
	Environment      string
	WorkspaceID      string

	// FounderUsername/FounderPassword configure the single founder identity
	// (AUSTRO_FOUNDER_USERNAME / AUSTRO_FOUNDER_PASSWORD). Both must be set
	// together; until then the bootstrap endpoint is disabled (409). The values
	// are deployment configuration, never hardcoded, and the password is never
	// logged or echoed.
	FounderUsername string
	FounderPassword string

	// AIBackend selects the AI Provider adapter (AIBackendStub by default).
	// A non-stub backend is CONFIGURATION_REQUIRED: AIModel, AIBaseURL and
	// AIAPIKey must be present and secure, or validation fails fast.
	AIBackend string
	// AIModel is the model identifier used by a non-stub AI backend.
	AIModel string
	// AIBaseURL is the OpenAI-compatible endpoint base URL (non-stub only).
	AIBaseURL string
	// AIAPIKey is the credential for the AI backend (non-stub only). It is
	// never logged and never echoed into validation errors.
	AIAPIKey string
	// AIUsageLimitPerWorkspace caps total AI operations per workspace (0 =
	// no ceiling; forwarded to the usage guard).
	AIUsageLimitPerWorkspace uint64
	// usageLimitRaw is the raw environment value for AIUsageLimitPerWorkspace.
	// Strict validation rejects a value that does not parse as an unsigned
	// integer (fail-fast) rather than silently treating it as "no ceiling".
	usageLimitRaw string

	// PublishBackend selects the Publisher adapter (PublishBackendStub by
	// default). A non-stub backend is CONFIGURATION_REQUIRED: PublishWebhookURL
	// and PublishToken must be present and secure.
	PublishBackend string
	// PublishWebhookURL is the external delivery endpoint (non-stub only).
	PublishWebhookURL string
	// PublishToken is the bearer credential for the delivery endpoint
	// (non-stub only). It is never logged.
	PublishToken string
	// PublishIdempotencyField names the provider-specific header/query field
	// used for delivery idempotency (non-stub only). Optional: when empty, the
	// generic HTTP adapter derives a deterministic key but does not attach a
	// provider-specific field.
	PublishIdempotencyField string
	// PublishMaxAttempts bounds total delivery attempts (>= 1; non-stub only).
	// Zero uses the adapter's single-attempt default (no retry).
	PublishMaxAttempts int
	// PublishRetryBackoffBase/PublishRetryBackoffMax bound the retry backoff
	// pacing (non-stub only). Zero uses the adapter defaults.
	PublishRetryBackoffBase time.Duration
	PublishRetryBackoffMax  time.Duration

	// Raw environment text for the settings above. Strict validation rejects a
	// value that does not parse rather than silently substituting the default,
	// because a typo in a retry budget would otherwise be indistinguishable
	// from an intentionally unset one.
	publishMaxAttemptsRaw string
	publishBackoffBaseRaw string
	publishBackoffMaxRaw  string

	// AI execution runtime. These govern the dispatcher/worker loops that carry
	// claimed retries from the admit board to the configured AI provider.
	//
	// ExecutionTransportBackend selects how the dispatcher delivers a claimed
	// retry (ExecutionTransportBackendStub by default; a broker-backed backend
	// is CONFIGURATION_REQUIRED through the entrypoint adapter).
	ExecutionTransportBackend string
	// ExecutionQueue is the durable broker queue the dispatcher publishes each
	// claimed retry to and the worker consumes (AUSTRO_EXECUTION_QUEUE,
	// default "austro.executions"). The two processes are separate binaries
	// and never share memory, so this name is the only thing that makes them
	// meet: it is deployment configuration, never a hardcoded constant at
	// either end, because a rename on one side alone would produce a
	// dispatcher publishing into a void while both processes report healthy.
	ExecutionQueue string
	// ExecutionLeaseTTL is the lease granted on each claim/reclaim and renewed
	// by each heartbeat. It bounds how long a crashed worker's work blocks
	// recovery.
	ExecutionLeaseTTL time.Duration
	// ExecutionHeartbeatInterval is how often a running worker renews its lease.
	// It must be strictly under the lease so a healthy worker never looks stale.
	ExecutionHeartbeatInterval time.Duration
	// ExecutionWindow caps the non-heartbeat provider call budget inside a lease.
	ExecutionWindow time.Duration
	// ExecutionSweepInterval paces each workspace dispatch/recovery sweep.
	ExecutionSweepInterval time.Duration
	// ExecutionRetryBackoffBase/ExecutionRetryBackoffMax bound the retry backoff
	// pacing (RetryDelay) for scheduled attempts.
	ExecutionRetryBackoffBase time.Duration
	ExecutionRetryBackoffMax  time.Duration
	// ExecutionWorkspaceIDs names the workspaces the dispatcher sweep loop runs
	// for (AUSTRO_EXECUTION_WORKSPACE_IDS, comma-separated UUIDs). When empty no
	// dispatch loop is started.
	ExecutionWorkspaceIDs []uuid.UUID

	// AllowStubBackends (AUSTRO_ALLOW_STUB_BACKENDS) is the explicit opt-in that
	// permits the deterministic stub adapters — AI, publishing and the
	// execution transport — to run while AUSTRO_ENV declares production. It
	// exists so a genuinely offline production-shaped stack (a demo, a
	// rehearsal, a site with no egress) can be deployed without pretending the
	// stubs are real, and so the refusal in production is a decision someone
	// can find and undo in the environment file rather than a hardcoded rule
	// nobody can see. Default false, and an unparseable value is rejected
	// rather than read as consent.
	AllowStubBackends    bool
	allowStubBackendsRaw string

	// Raw environment text for the settings above, kept for the same fail-fast
	// discipline as the publishing settings.
	executionLeaseTTLRaw            string
	executionHeartbeatIntervalRaw   string
	executionWindowRaw              string
	executionSweepIntervalRaw       string
	executionRetryBackoffBaseRaw    string
	executionRetryBackoffMaxRaw     string
	executionWorkspaceIDsRaw        string
}

// Minimum accepted lengths for credentials.
const (
	// minHMACKeyBytes is the floor for the JWT signing secrets. RFC 7518 §3.2
	// requires a key used with a MAC algorithm to be at least as long as the
	// hash output, so HS256 needs 32 bytes; a shorter key is below the
	// algorithm's design strength regardless of how random it looks.
	minHMACKeyBytes = 32
	// minFounderPasswordBytes is the floor for the single founder credential.
	// It is the only interactive credential in the system and gates
	// organization-level authority, so it is held to a password floor rather
	// than a key floor.
	minFounderPasswordBytes = 16
)

var globalConfig *Config

// ErrConfigInvalid is returned when required configuration is missing or set
// to an insecure placeholder/default. The error never exposes secret values.
var ErrConfigInvalid = errors.New("invalid configuration")

func defaults() *Config {
	usageLimit := uint64(0)
	usageLimitRaw := os.Getenv("AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE")
	if usageLimitRaw != "" {
		if v, err := strconv.ParseUint(usageLimitRaw, 10, 64); err == nil {
			usageLimit = v
		}
	}
	ownerDSN := getEnv("AUSTRO_POSTGRES_DSN", "postgres://austro:austro@localhost:5432/austro?sslmode=disable")

	preProvisionedRolesRaw := os.Getenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES")
	preProvisionedRoles := false
	if preProvisionedRolesRaw != "" {
		if b, err := strconv.ParseBool(preProvisionedRolesRaw); err == nil {
			preProvisionedRoles = b
		}
	}
	allowStubBackendsRaw := os.Getenv("AUSTRO_ALLOW_STUB_BACKENDS")
	allowStubBackends := false
	if allowStubBackendsRaw != "" {
		if b, err := strconv.ParseBool(allowStubBackendsRaw); err == nil {
			allowStubBackends = b
		}
	}
	// In pre-provisioned mode the runtime DSN must be stated explicitly: the
	// role already exists with the platform's own credential, so deriving a
	// DSN from the owner (owner's password, guessed role name) would be a
	// credential the server cannot authenticate. An empty value is rejected
	// by Validate instead of being silently guessed.
	var runtimeDSN string
	if preProvisionedRoles {
		runtimeDSN = os.Getenv("AUSTRO_POSTGRES_RUNTIME_DSN")
	} else {
		// Derived rather than defaulted to a constant so there is exactly
		// one place a deployment states its database location, and so
		// forgetting the runtime DSN lands on the unprivileged role instead
		// of quietly serving traffic as the schema owner.
		runtimeDSN = getEnv("AUSTRO_POSTGRES_RUNTIME_DSN", DeriveRuntimeDSN(ownerDSN))
	}
	return &Config{
		ServerAddress:               getEnv("AUSTRO_SERVER_ADDR", "0.0.0.0:8080"),
		PostgresDSN:                 ownerDSN,
		PostgresRuntimeDSN:          runtimeDSN,
		PostgresAdminDSN:            os.Getenv("AUSTRO_POSTGRES_ADMIN_DSN"),
		PostgresPreProvisionedRoles: preProvisionedRoles,
		preProvisionedRolesRaw:      preProvisionedRolesRaw,
		RedisAddr:                   getEnv("AUSTRO_REDIS_ADDR", "localhost:6379"),
		RabbitMQURL:                 getEnv("AUSTRO_RABBITMQ_URL", "amqp://austro:austro@localhost:5672"),
		RabbitMQQueue:               getEnv("AUSTRO_RABBITMQ_QUEUE", "austro.events"),
		JWTSecret:                   getEnv("AUSTRO_JWT_SECRET", "change-me-in-production"),
		JWTRefreshSecret:            getEnv("AUSTRO_JWT_REFRESH_SECRET", "change-me-in-production"),
		Environment:                 getEnv("AUSTRO_ENV", "development"),
		WorkspaceID:                 getEnv("AUSTRO_WORKSPACE_ID", "default"),

		FounderUsername: os.Getenv("AUSTRO_FOUNDER_USERNAME"),
		FounderPassword: os.Getenv("AUSTRO_FOUNDER_PASSWORD"),

		AIBackend:                getEnv("AUSTRO_AI_BACKEND", AIBackendStub),
		AIModel:                  os.Getenv("AUSTRO_AI_MODEL"),
		AIBaseURL:                os.Getenv("AUSTRO_AI_BASE_URL"),
		AIAPIKey:                 os.Getenv("AUSTRO_AI_API_KEY"),
		AIUsageLimitPerWorkspace: usageLimit,
		usageLimitRaw:            usageLimitRaw,

		PublishBackend:          getEnv("AUSTRO_PUBLISH_BACKEND", PublishBackendStub),
		PublishWebhookURL:       os.Getenv("AUSTRO_PUBLISH_WEBHOOK_URL"),
		PublishToken:            os.Getenv("AUSTRO_PUBLISH_TOKEN"),
		PublishIdempotencyField: os.Getenv("AUSTRO_PUBLISH_IDEMPOTENCY_FIELD"),
		PublishMaxAttempts:      envInt("AUSTRO_PUBLISH_MAX_ATTEMPTS", 0),
		PublishRetryBackoffBase: envDuration("AUSTRO_PUBLISH_RETRY_BACKOFF_BASE", 0),
		PublishRetryBackoffMax:  envDuration("AUSTRO_PUBLISH_RETRY_BACKOFF_MAX", 0),

		publishMaxAttemptsRaw: os.Getenv("AUSTRO_PUBLISH_MAX_ATTEMPTS"),
		publishBackoffBaseRaw: os.Getenv("AUSTRO_PUBLISH_RETRY_BACKOFF_BASE"),
		publishBackoffMaxRaw:  os.Getenv("AUSTRO_PUBLISH_RETRY_BACKOFF_MAX"),

		ExecutionTransportBackend:     getEnv("AUSTRO_EXECUTION_TRANSPORT_BACKEND", ExecutionTransportBackendStub),
		ExecutionQueue:                getEnv("AUSTRO_EXECUTION_QUEUE", DefaultExecutionQueue),
		ExecutionLeaseTTL:             envDuration("AUSTRO_EXECUTION_LEASE_TTL", 60*time.Second),
		ExecutionHeartbeatInterval:    envDuration("AUSTRO_EXECUTION_HEARTBEAT_INTERVAL", 20*time.Second),
		ExecutionWindow:               envDuration("AUSTRO_EXECUTION_WINDOW", 50*time.Second),
		ExecutionSweepInterval:        envDuration("AUSTRO_EXECUTION_SWEEP_INTERVAL", 5*time.Second),
		ExecutionRetryBackoffBase:     envDuration("AUSTRO_EXECUTION_RETRY_BACKOFF_BASE", 2*time.Second),
		ExecutionRetryBackoffMax:      envDuration("AUSTRO_EXECUTION_RETRY_BACKOFF_MAX", 2*time.Minute),
		ExecutionWorkspaceIDs:         envUUIDs("AUSTRO_EXECUTION_WORKSPACE_IDS"),
		AllowStubBackends:             allowStubBackends,
		allowStubBackendsRaw:          allowStubBackendsRaw,

		executionLeaseTTLRaw:          os.Getenv("AUSTRO_EXECUTION_LEASE_TTL"),
		executionHeartbeatIntervalRaw: os.Getenv("AUSTRO_EXECUTION_HEARTBEAT_INTERVAL"),
		executionWindowRaw:            os.Getenv("AUSTRO_EXECUTION_WINDOW"),
		executionSweepIntervalRaw:     os.Getenv("AUSTRO_EXECUTION_SWEEP_INTERVAL"),
		executionRetryBackoffBaseRaw:  os.Getenv("AUSTRO_EXECUTION_RETRY_BACKOFF_BASE"),
		executionRetryBackoffMaxRaw:   os.Getenv("AUSTRO_EXECUTION_RETRY_BACKOFF_MAX"),
		executionWorkspaceIDsRaw:      os.Getenv("AUSTRO_EXECUTION_WORKSPACE_IDS"),
	}
}

// Load reads configuration from the environment, falling back to development
// defaults for callers that intentionally run outside a configured runtime
// (tests). Runtime entrypoints must use LoadStrict to fail fast.
func Load() *Config {
	globalConfig = defaults()
	return globalConfig
}

func Get() *Config {
	if globalConfig == nil {
		return Load()
	}
	return globalConfig
}

// LoadStrict reads configuration without insecure fallbacks and fails fast if
// any required setting is missing or set to a placeholder/default. It returns
// a structured error naming the affected settings (never their values).
func LoadStrict() (*Config, error) {
	cfg := defaults()
	return cfg, cfg.Validate()
}

// Validate fails fast on missing, empty, or insecure required settings. The
// returned error lists setting names only; no secret values are exposed.
func (c *Config) Validate() error {
	var missing []string
	for name, value := range map[string]string{
		"AUSTRO_POSTGRES_DSN": c.PostgresDSN,
		"AUSTRO_REDIS_ADDR":   c.RedisAddr,
		"AUSTRO_RABBITMQ_URL": c.RabbitMQURL,
	} {
		if isInsecure(value) {
			missing = append(missing, name)
		}
	}

	// The runtime DSN is optional (it is derived from the owner DSN when
	// absent), so it is only checked when explicitly supplied. Two rules
	// apply: it must not be a placeholder, and it must not be the owner DSN.
	// Running application traffic as the schema owner is the exact defect
	// this setting exists to prevent, and PostgreSQL silently skips row level
	// security for a table's owner, so an owner connection would look
	// correctly configured while enforcing nothing.
	if c.PostgresRuntimeDSN != "" {
		if isInsecure(c.PostgresRuntimeDSN) {
			missing = append(missing, "AUSTRO_POSTGRES_RUNTIME_DSN")
		}
		if c.PostgresRuntimeDSN == c.PostgresDSN {
			missing = append(missing, "AUSTRO_POSTGRES_RUNTIME_DSN")
		}
	}

	// Pre-provisioned mode: the topology was created outside this process, so
	// both serving principals must be stated with their real credentials. A
	// missing or placeholder DSN, or an admin DSN that collapses onto the
	// owner or the runtime, is rejected here at configuration time rather
	// than discovered at the first request. Role-level collapse (same role,
	// different credential) is caught later by the topology resolver, which
	// sees the parsed role names.
	// The flag itself must parse as a boolean no matter what it parsed to:
	// a typo like "true " would otherwise fall through as "off" and the
	// pre-provisioned topology would silently go unverified.
	if c.preProvisionedRolesRaw != "" {
		if _, err := strconv.ParseBool(c.preProvisionedRolesRaw); err != nil {
			missing = append(missing, "AUSTRO_POSTGRES_PREPROVISIONED_ROLES")
		}
	}

	if c.PostgresPreProvisionedRoles {
		if c.PostgresRuntimeDSN == "" || isInsecure(c.PostgresRuntimeDSN) {
			missing = append(missing, "AUSTRO_POSTGRES_RUNTIME_DSN")
		}
		if c.PostgresAdminDSN == "" || isInsecure(c.PostgresAdminDSN) {
			missing = append(missing, "AUSTRO_POSTGRES_ADMIN_DSN")
		}
		if c.PostgresAdminDSN != "" && c.PostgresAdminDSN == c.PostgresDSN {
			missing = append(missing, "AUSTRO_POSTGRES_ADMIN_DSN")
		}
		if c.PostgresAdminDSN != "" && c.PostgresAdminDSN == c.PostgresRuntimeDSN {
			missing = append(missing, "AUSTRO_POSTGRES_ADMIN_DSN")
		}
	}

	// The JWT secrets carry an additional floor: a placeholder check alone
	// would accept a one-byte secret, which is below the design strength of
	// HS256 (RFC 7518 §3.2 requires the key to be at least the hash length).
	for name, value := range map[string]string{
		"AUSTRO_JWT_SECRET":         c.JWTSecret,
		"AUSTRO_JWT_REFRESH_SECRET": c.JWTRefreshSecret,
	} {
		if isInsecure(value) || len(value) < minHMACKeyBytes {
			missing = append(missing, name)
		}
	}

	missing = append(missing, c.validateBackends()...)

	if c.usageLimitRaw != "" {
		if _, err := strconv.ParseUint(c.usageLimitRaw, 10, 64); err != nil {
			missing = append(missing, "AUSTRO_AI_USAGE_LIMIT_PER_WORKSPACE")
		}
	}

	// A value that was supplied but does not parse is a configuration error,
	// not a request to use the default. envInt/envDuration fall back so the
	// value is never half-applied; validation is what turns that into a
	// fail-fast.
	for name, raw := range map[string]string{
		"AUSTRO_PUBLISH_MAX_ATTEMPTS":       c.publishMaxAttemptsRaw,
		"AUSTRO_PUBLISH_RETRY_BACKOFF_BASE": c.publishBackoffBaseRaw,
		"AUSTRO_PUBLISH_RETRY_BACKOFF_MAX":  c.publishBackoffMaxRaw,
	} {
		if raw == "" {
			continue
		}
		if name == "AUSTRO_PUBLISH_MAX_ATTEMPTS" {
			if _, err := strconv.Atoi(raw); err != nil {
				missing = append(missing, name)
			}
			continue
		}
		if _, err := time.ParseDuration(raw); err != nil {
			missing = append(missing, name)
		}
	}

	if c.PublishMaxAttempts < 0 {
		missing = append(missing, "AUSTRO_PUBLISH_MAX_ATTEMPTS")
	}
	if c.PublishRetryBackoffBase < 0 || c.PublishRetryBackoffMax < 0 {
		missing = append(missing, "AUSTRO_PUBLISH_RETRY_BACKOFF")
	}

	// AI execution runtime settings. Supplied duration values must parse, the
	// resolved durations must be positive (a zero lease or ticker interval would
	// panic the dispatcher/worker loops), and the pacing relationships the
	// runtime relies on must hold: the heartbeat and the provider call window
	// must stay strictly under the lease so a healthy worker never looks stale
	// and never lets the provider call outlive its fence; the backoff base must
	// not exceed the cap.
	for name, raw := range map[string]string{
		"AUSTRO_EXECUTION_LEASE_TTL":          c.executionLeaseTTLRaw,
		"AUSTRO_EXECUTION_HEARTBEAT_INTERVAL": c.executionHeartbeatIntervalRaw,
		"AUSTRO_EXECUTION_WINDOW":             c.executionWindowRaw,
		"AUSTRO_EXECUTION_SWEEP_INTERVAL":     c.executionSweepIntervalRaw,
		"AUSTRO_EXECUTION_RETRY_BACKOFF_BASE": c.executionRetryBackoffBaseRaw,
		"AUSTRO_EXECUTION_RETRY_BACKOFF_MAX":  c.executionRetryBackoffMaxRaw,
	} {
		if raw == "" {
			continue
		}
		if _, err := time.ParseDuration(raw); err != nil {
			missing = append(missing, name)
		}
	}
	if c.executionWorkspaceIDsRaw != "" {
		for _, token := range strings.Split(c.executionWorkspaceIDsRaw, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if _, err := uuid.Parse(token); err != nil {
				missing = append(missing, "AUSTRO_EXECUTION_WORKSPACE_IDS")
			}
		}
	}
	if c.ExecutionLeaseTTL <= 0 {
		missing = append(missing, "AUSTRO_EXECUTION_LEASE_TTL")
	}
	if c.ExecutionHeartbeatInterval <= 0 {
		missing = append(missing, "AUSTRO_EXECUTION_HEARTBEAT_INTERVAL")
	}
	if c.ExecutionWindow <= 0 {
		missing = append(missing, "AUSTRO_EXECUTION_WINDOW")
	}
	if c.ExecutionSweepInterval <= 0 {
		missing = append(missing, "AUSTRO_EXECUTION_SWEEP_INTERVAL")
	}
	if c.ExecutionRetryBackoffBase <= 0 {
		missing = append(missing, "AUSTRO_EXECUTION_RETRY_BACKOFF_BASE")
	}
	if c.ExecutionRetryBackoffMax <= 0 {
		missing = append(missing, "AUSTRO_EXECUTION_RETRY_BACKOFF_MAX")
	}
	if c.ExecutionHeartbeatInterval >= c.ExecutionLeaseTTL {
		missing = append(missing, "AUSTRO_EXECUTION_HEARTBEAT_INTERVAL")
	}
	if c.ExecutionWindow >= c.ExecutionLeaseTTL {
		missing = append(missing, "AUSTRO_EXECUTION_WINDOW")
	}
	if c.ExecutionRetryBackoffBase > c.ExecutionRetryBackoffMax {
		missing = append(missing, "AUSTRO_EXECUTION_RETRY_BACKOFF")
	}

	// Founder bootstrap is optional, but both settings must be supplied
	// together (a half-configured bootstrap would fail at runtime), and a
	// configured password must never be a placeholder or shorter than the
	// password floor.
	if (c.FounderUsername == "") != (c.FounderPassword == "") {
		missing = append(missing, "AUSTRO_FOUNDER_USERNAME/AUSTRO_FOUNDER_PASSWORD")
	}
	if c.FounderPassword != "" && (isInsecure(c.FounderPassword) || len(c.FounderPassword) < minFounderPasswordBytes) {
		missing = append(missing, "AUSTRO_FOUNDER_PASSWORD")
	}

	if len(missing) > 0 {
		// Sorted and de-duplicated so the same misconfiguration always produces
		// the same message instead of one in random map order.
		return fmt.Errorf("%w: required settings missing or insecure: %s", ErrConfigInvalid, strings.Join(sortedUnique(missing), ", "))
	}
	return nil
}

// sortedUnique returns names in stable order with repeats collapsed. Several
// rules can flag the same setting, and the error should name it once.
func sortedUnique(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// validateBackends enforces the cross-field rules for the AI and publishing
// adapter selection. The deterministic stub adapters require no external
// configuration; selecting any real backend is CONFIGURATION_REQUIRED and its
// settings must be present and secure, or validation fails fast. Supplying
// external settings while the stub backend is selected is rejected so a
// configured credential is never silently ignored.
func (c *Config) validateBackends() []string {
	var missing []string

	switch c.AIBackend {
	case "", AIBackendStub:
		// Offline adapter: no external credential may be present.
		for name, value := range map[string]string{
			"AUSTRO_AI_MODEL":    c.AIModel,
			"AUSTRO_AI_BASE_URL": c.AIBaseURL,
			"AUSTRO_AI_API_KEY":  c.AIAPIKey,
		} {
			if value != "" {
				missing = append(missing, name)
			}
		}
	case AIBackendLocal:
		// Local/free keyless OpenAI-compatible endpoint. Model and BaseURL are
		// CONFIGURATION_REQUIRED; the API key is optional (local endpoints often
		// need no credential). A supplied key must still be secure, and a
		// placeholder is rejected the same way it is for openai-compatible.
		for name, value := range map[string]string{
			"AUSTRO_AI_MODEL":    c.AIModel,
			"AUSTRO_AI_BASE_URL": c.AIBaseURL,
		} {
			if isInsecure(value) {
				missing = append(missing, name)
			}
		}
		if c.AIAPIKey != "" && isInsecure(c.AIAPIKey) {
			missing = append(missing, "AUSTRO_AI_API_KEY")
		}
	case AIBackendOpenAICompatible:
		for name, value := range map[string]string{
			"AUSTRO_AI_MODEL":    c.AIModel,
			"AUSTRO_AI_BASE_URL": c.AIBaseURL,
			"AUSTRO_AI_API_KEY":  c.AIAPIKey,
		} {
			if isInsecure(value) {
				missing = append(missing, name)
			}
		}
	default:
		missing = append(missing, "AUSTRO_AI_BACKEND")
	}

	switch c.PublishBackend {
	case "", PublishBackendStub:
		for name, value := range map[string]string{
			"AUSTRO_PUBLISH_WEBHOOK_URL": c.PublishWebhookURL,
			"AUSTRO_PUBLISH_TOKEN":       c.PublishToken,
		} {
			if value != "" {
				missing = append(missing, name)
			}
		}
	case PublishBackendGenericHTTP:
		for name, value := range map[string]string{
			"AUSTRO_PUBLISH_WEBHOOK_URL": c.PublishWebhookURL,
			"AUSTRO_PUBLISH_TOKEN":       c.PublishToken,
		} {
			if isInsecure(value) {
				missing = append(missing, name)
			}
		}
	default:
		missing = append(missing, "AUSTRO_PUBLISH_BACKEND")
	}

	switch c.ExecutionTransportBackend {
	case "", ExecutionTransportBackendStub:
		// Offline transport: no broker configuration may be demanded at rest.
	case ExecutionTransportBackendRabbitMQ:
		// Broker-backed: the entrypoint dials the configured RabbitMQ URL; there
		// is no resting placeholder that could be mistaken for a live broker.
	default:
		missing = append(missing, "AUSTRO_EXECUTION_TRANSPORT_BACKEND")
	}

	// The execution queue name is the only rendezvous between the dispatcher
	// (in the API process) and the worker (in its own process). A blank or
	// whitespace-only name would be declared by one side and never bound by the
	// other, so it is rejected at startup rather than defaulted at the point of
	// use where the two sides would each substitute a different value.
	if strings.TrimSpace(c.ExecutionQueue) == "" {
		missing = append(missing, "AUSTRO_EXECUTION_QUEUE")
	}

	// The opt-in must be readable. A typo that failed open would grant exactly
	// the exemption the operator did not intend, so it fails closed.
	if c.allowStubBackendsRaw != "" {
		if _, err := strconv.ParseBool(c.allowStubBackendsRaw); err != nil {
			missing = append(missing, "AUSTRO_ALLOW_STUB_BACKENDS")
		}
	}

	// Production must not run a deterministic stub. Every stub adapter answers
	// successfully without contacting a real service, so a deployment that
	// reached production on the offline defaults would serve traffic, answer
	// /health/ready, and perform no real work at all — the most expensive kind
	// of misconfiguration, because nothing in the system reports it. The rule
	// is scoped to the production environment so the offline development
	// default is untouched, and it yields to the explicit, greppable opt-in.
	if c.isProduction() && !c.AllowStubBackends {
		switch c.AIBackend {
		case "", AIBackendStub:
			missing = append(missing, "AUSTRO_AI_BACKEND")
		}
		switch c.PublishBackend {
		case "", PublishBackendStub:
			missing = append(missing, "AUSTRO_PUBLISH_BACKEND")
		}
		switch c.ExecutionTransportBackend {
		case "", ExecutionTransportBackendStub:
			missing = append(missing, "AUSTRO_EXECUTION_TRANSPORT_BACKEND")
		}
	}

	// A broker-backed transport with no workspaces to sweep is a dispatcher
	// that connects, logs "execution-dispatch-started" and then never dispatches
	// anything. In production that is a silently broken subsystem wearing the
	// costume of a working one, so it is a startup failure. In development it
	// stays a deliberate no-op: running the API without the execution pipeline
	// is the normal way to work on anything else.
	if c.isProduction() && c.ExecutionTransportBackend == ExecutionTransportBackendRabbitMQ && len(c.ExecutionWorkspaceIDs) == 0 {
		missing = append(missing, "AUSTRO_EXECUTION_WORKSPACE_IDS")
	}

	return missing
}

// isProduction reports whether the configuration declares the production
// environment. The comparison trims and folds case so that "Production" or a
// stray trailing space cannot slip past a production-only rule: an
// environment label that only half activates its safety rules is worse than
// one that never activates them, because it looks configured.
func (c *Config) isProduction() bool {
	return strings.EqualFold(strings.TrimSpace(c.Environment), EnvironmentProduction)
}

// MustLoad is the runtime startup gate: it validates required configuration
// and, on any missing or insecure setting, emits a structured error and
// terminates instead of merely warning (fail-fast).
func MustLoad(cfg *Config) {
	if err := cfg.Validate(); err != nil {
		logger.NewEntry("invalid-configuration").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}
}

// isInsecure reports whether a required setting is absent or still set to one
// of the development placeholders/defaults that must never be used at runtime.
// DeriveRuntimeDSN rewrites an owner DSN into the unprivileged application
// runtime DSN: same host, port, database and query options, with the role
// replaced by AUSTRO_POSTGRES_RUNTIME_USER (default "austro_app") and the
// password replaced by AUSTRO_POSTGRES_RUNTIME_PASSWORD when that is set.
//
// A DSN that cannot be parsed yields an empty string rather than a guess:
// Validate then rejects the configuration instead of the process silently
// connecting as the owner.
func DeriveRuntimeDSN(ownerDSN string) string {
	u, err := url.Parse(ownerDSN)
	if err != nil || u.User == nil {
		return ""
	}
	user := getEnv("AUSTRO_POSTGRES_RUNTIME_USER", "austro_app")
	password, hasPassword := u.User.Password()
	if override := os.Getenv("AUSTRO_POSTGRES_RUNTIME_PASSWORD"); override != "" {
		password, hasPassword = override, true
	}
	if hasPassword {
		u.User = url.UserPassword(user, password)
	} else {
		u.User = url.User(user)
	}
	return u.String()
}

func isInsecure(v string) bool {
	if v == "" {
		return true
	}
	lower := strings.ToLower(v)
	for _, bad := range []string{"change-me", "changeme", "localhost:5432", "localhost:6379", "localhost:5672", "example"} {
		if strings.Contains(lower, bad) {
			return true
		}
	}
	return false
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// envInt reads an integer environment value, returning fallback for an empty
// or non-integer value. Validation (Validate) rejects a negative attempt count.
func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

// envDuration reads a Go duration string (e.g. "500ms", "30s") returning
// fallback for an empty or unparseable value.
func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return v
}

// envUUIDs reads a comma-separated list of UUIDs, dropping empty tokens. A
// token that does not parse is dropped here and rejected by Validate, which
// keeps the parsed field and the fail-fast rule from disagreeing about whether
// a malformed value was configured.
func envUUIDs(key string) []uuid.UUID {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	var out []uuid.UUID
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if id, err := uuid.Parse(token); err == nil {
			out = append(out, id)
		}
	}
	return out
}
