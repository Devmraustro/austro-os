package database

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// The database runs three distinct principals, and the separation is the whole
// point of the row level security design:
//
//	owner    (AUSTRO_POSTGRES_DSN)         creates extensions, tables, indexes,
//	                                       policies and roles. Owns the tables.
//	                                       Used only during bootstrap and
//	                                       migration, then closed.
//	runtime  (AUSTRO_POSTGRES_RUNTIME_DSN) serves every request. Not a
//	                                       superuser, no BYPASSRLS, owns
//	                                       nothing, so the workspace policies
//	                                       genuinely constrain it.
//	admin    (AUSTRO_POSTGRES_ADMIN_USER)  two narrow organization-level
//	                                       operations that cannot be expressed
//	                                       by a workspace-scoped policy:
//	                                       founder workspace listing/creation,
//	                                       and appending to / verifying the
//	                                       audit chain. Also not a superuser
//	                                       and no BYPASSRLS: its wider reach
//	                                       comes from explicit policies that
//	                                       name it, never from skipping row
//	                                       level security.
//
// Nothing here grants superuser or BYPASSRLS. A principal that had either
// would make every policy in this file decorative, because PostgreSQL applies
// neither ENABLE nor FORCE ROW LEVEL SECURITY to such a role.

// adminRoleName returns the configured administrative role, defaulting to
// "<runtime role>_admin" so the three principals are visibly related and no
// deployment has to invent a second name.
func adminRoleName(runtimeRole string) string {
	if v := os.Getenv("AUSTRO_POSTGRES_ADMIN_USER"); v != "" {
		return v
	}
	return runtimeRole + "_admin"
}

// validRole matches the identifiers this file is willing to interpolate into
// DDL. Role names cannot be passed as bind parameters, so they are checked
// against an explicit pattern instead of being quoted by hand.
var validRole = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// roleFromDSN extracts the role a DSN authenticates as.
func roleFromDSN(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	if u.User == nil || u.User.Username() == "" {
		return "", fmt.Errorf("dsn carries no role")
	}
	return u.User.Username(), nil
}

// Topology is the resolved set of database principals for this process.
type Topology struct {
	OwnerDSN   string
	RuntimeDSN string
	AdminDSN   string
	Runtime    string
	Admin      string
}

// ResolveTopology derives the three principals from configuration. The runtime
// DSN is derived from the owner DSN when it is not set explicitly, so a process
// with no extra configuration still lands on the unprivileged role instead of
// quietly serving traffic as the table owner.
//
// It is an error for the runtime principal to be the owner principal: that is
// the exact misconfiguration the split exists to prevent, and it is rejected
// here rather than discovered later as a policy that never applied.
func ResolveTopology(ownerDSN, runtimeDSN string) (*Topology, error) {
	ownerRole, err := roleFromDSN(ownerDSN)
	if err != nil {
		return nil, fmt.Errorf("owner dsn: %w", err)
	}
	if runtimeDSN == "" {
		runtimeDSN = deriveRuntimeDSN(ownerDSN)
	}
	runtimeRole, err := roleFromDSN(runtimeDSN)
	if err != nil {
		return nil, fmt.Errorf("runtime dsn: %w", err)
	}
	if strings.EqualFold(runtimeRole, ownerRole) {
		return nil, fmt.Errorf(
			"application runtime role %q must differ from the schema owner %q: "+
				"PostgreSQL does not apply row level security to a table's owner, "+
				"so an owner connection enforces no workspace isolation",
			runtimeRole, ownerRole)
	}
	for _, r := range []string{ownerRole, runtimeRole} {
		if !validRole.MatchString(r) {
			return nil, fmt.Errorf("role name %q is not a supported identifier", r)
		}
	}
	admin := adminRoleName(runtimeRole)
	if !validRole.MatchString(admin) {
		return nil, fmt.Errorf("admin role name %q is not a supported identifier", admin)
	}
	adminDSN, err := withRole(runtimeDSN, admin)
	if err != nil {
		return nil, err
	}
	return &Topology{
		OwnerDSN:   ownerDSN,
		RuntimeDSN: runtimeDSN,
		AdminDSN:   adminDSN,
		Runtime:    runtimeRole,
		Admin:      admin,
	}, nil
}

// deriveRuntimeDSN rewrites an owner DSN onto the unprivileged runtime role,
// reusing the owner password unless AUSTRO_POSTGRES_RUNTIME_PASSWORD supplies a
// dedicated one. A deployment should set both explicitly; the reuse exists so
// a developer who sets nothing still gets the unprivileged path.
func deriveRuntimeDSN(ownerDSN string) string {
	role := os.Getenv("AUSTRO_POSTGRES_RUNTIME_USER")
	if role == "" {
		role = "austro_app"
	}
	out, err := withRole(ownerDSN, role)
	if err != nil {
		return ""
	}
	if pw := os.Getenv("AUSTRO_POSTGRES_RUNTIME_PASSWORD"); pw != "" {
		if rewritten, err := withRolePassword(ownerDSN, role, pw); err == nil {
			return rewritten
		}
	}
	return out
}

func withRole(dsn, role string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	if u.User == nil {
		return "", fmt.Errorf("dsn carries no role")
	}
	pw, ok := u.User.Password()
	if ok {
		u.User = url.UserPassword(role, pw)
	} else {
		u.User = url.User(role)
	}
	return u.String(), nil
}

func withRolePassword(dsn, role, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.User = url.UserPassword(role, password)
	return u.String(), nil
}

// passwordFromDSN returns the password a DSN authenticates with, so the
// bootstrap can provision the role it is about to connect as.
func passwordFromDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}

// quoteLit escapes a value for inclusion inside a single-quoted SQL literal.
// Only used for role passwords: ALTER ROLE ... PASSWORD cannot be bound as a
// parameter.
func quoteLit(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// provisionRoles creates (or corrects) the runtime and admin principals and
// grants them exactly the privileges the application needs. It runs on the
// owner connection and is idempotent, so it is safe on every boot and after
// every migration that adds a table.
//
// The privileges are deliberately re-asserted in full each time: a grant made
// once would not cover a table created by a later migration, and a silent gap
// there surfaces as a confusing "permission denied" in production rather than
// as a security problem, which makes it easy to "fix" by reaching for the owner
// connection.
func provisionRoles(db *sql.DB, topo *Topology) error {
	// NOSUPERUSER / NOBYPASSRLS are asserted on every boot rather than only at
	// creation. A role altered out-of-band by an operator (or by a migration
	// tool) is corrected here, and if it cannot be, the runtime verification
	// that follows refuses to serve.
	for _, role := range []string{topo.Runtime, topo.Admin} {
		stmt := fmt.Sprintf(`
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
				EXECUTE 'CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE';
			ELSE
				EXECUTE 'ALTER ROLE %s NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE';
			END IF;
		END$$;`, quoteLit(role), role, role)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("provision role %s: %w", role, err)
		}
	}

	// Passwords are set from the DSNs the process will actually authenticate
	// with, so the roles this function creates are the roles it can connect as.
	for _, pair := range []struct{ role, dsn string }{
		{topo.Runtime, topo.RuntimeDSN},
		{topo.Admin, topo.AdminDSN},
	} {
		if pw := passwordFromDSN(pair.dsn); pw != "" {
			stmt := fmt.Sprintf("ALTER ROLE %s LOGIN PASSWORD %s", pair.role, quoteLit(pw))
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("set password for role %s: %w", pair.role, err)
			}
		}
	}

	// Runtime role: ordinary workspace-scoped DML on every table, then the
	// audit table narrowed to append-only. Revoking UPDATE and DELETE there is
	// what makes the audit log append-only from the application's own
	// connection, independent of any application-level discipline.
	grants := fmt.Sprintf(`
	GRANT USAGE ON SCHEMA public TO %s, %s;
	GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s;
	REVOKE UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON audit_events FROM %s;
	GRANT SELECT, INSERT ON audit_events TO %s;
	GRANT SELECT, INSERT ON workspaces TO %s;
	ALTER DEFAULT PRIVILEGES IN SCHEMA public
		GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s;
	ALTER DEFAULT PRIVILEGES IN SCHEMA public
		GRANT SELECT, INSERT ON TABLES TO %s;`,
		topo.Runtime, topo.Admin,
		topo.Runtime,
		topo.Runtime,
		topo.Admin,
		topo.Admin,
		topo.Runtime,
		topo.Admin,
	)
	if _, err := db.Exec(grants); err != nil {
		return fmt.Errorf("grant privileges: %w", err)
	}
	return nil
}

// assertOwnership returns every protected table to the schema owner.
//
// Row level security is not applied to a table's owner, and the runtime role
// must never own one, so ownership is a security property rather than
// bookkeeping. A migration tool or an operator can move ownership, and leaving
// it moved would mean a permanently unprotected table that only a startup
// assertion would notice. Re-asserting it on every boot makes the topology
// self-healing; VerifyRuntimeSecurity still checks the live state afterwards,
// so a repair that could not be applied is fatal rather than silent.
func assertOwnership(db *sql.DB, topo *Topology) error {
	ownerRole, err := roleFromDSN(topo.OwnerDSN)
	if err != nil {
		return fmt.Errorf("owner dsn: %w", err)
	}
	if !validRole.MatchString(ownerRole) {
		return fmt.Errorf("owner role name %q is not a supported identifier", ownerRole)
	}
	for _, table := range rlsTables() {
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s OWNER TO %s", table, ownerRole)); err != nil {
			return fmt.Errorf("set owner of %s to %s: %w", table, ownerRole, err)
		}
	}
	return nil
}

// forceRLS extends ENABLE ROW LEVEL SECURITY with FORCE on every protected
// table.
//
// ENABLE alone leaves a hole: PostgreSQL does not apply policies to the table's
// owner, so the owner connection is unconstrained. That is invisible in normal
// operation and becomes a total isolation failure the moment any code path,
// migration script or operator session uses the owner credential for traffic.
// FORCE closes it by applying the policies to the owner as well.
//
// It does not, and cannot, constrain a superuser or a role holding BYPASSRLS;
// both always bypass row level security. That is why the runtime role is
// created without either, and why VerifyRuntimeSecurity asserts the absence of
// both on the live connection rather than trusting this file.
//
// Superusers are unaffected in the other direction too, which is what keeps the
// administrative test fixtures working: they connect as the superuser and
// perform unbound seeding inserts that a forced policy would otherwise hide.
func forceRLS(db *sql.DB) error {
	for _, table := range rlsTables() {
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", table)); err != nil {
			return fmt.Errorf("force row level security on %s: %w", table, err)
		}
	}
	return nil
}
