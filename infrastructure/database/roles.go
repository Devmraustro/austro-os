package database

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
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

	// PreProvisioned records that the runtime and admin roles were expected
	// to exist on the server already (AUSTRO_POSTGRES_PREPROVISIONED_ROLES).
	// In that mode the bootstrap verifies these principals against the live
	// database and re-asserts the grants without ever creating a role or
	// touching a password; otherwise it self-provisions them.
	PreProvisioned bool
}

// ResolveTopology derives the three principals from configuration. The runtime
// DSN is derived from the owner DSN when it is not set explicitly, so a process
// with no extra configuration still lands on the unprivileged role instead of
// quietly serving traffic as the table owner. The administrative principal is
// derived the same way; ResolveTopologyWithAdmin is the form that also accepts
// an explicit admin DSN.
//
// It is an error for the runtime principal to be the owner principal: that is
// the exact misconfiguration the split exists to prevent, and it is rejected
// here rather than discovered later as a policy that never applied.
func ResolveTopology(ownerDSN, runtimeDSN string) (*Topology, error) {
	return ResolveTopologyWithAdmin(ownerDSN, runtimeDSN, "")
}

// ResolveTopologyWithAdmin is ResolveTopology with one addition: a dedicated
// administrative DSN. When adminDSN is supplied it is used exactly as given —
// its role name and credential are the operator's statement of fact, and
// deriving a different one would silently connect as the wrong principal. When
// it is empty the admin principal is derived from the runtime DSN exactly as
// before (role <runtime>_admin or AUSTRO_POSTGRES_ADMIN_USER, the runtime
// DSN's host/database/credential), so the classic self-provisioned deployment
// is bit-for-bit unchanged.
//
// Whatever the mode, the three principals must be three distinct roles: an
// admin that is the owner or the runtime concentrates a credential into a
// principal the security model assumes is separate, and the failure would
// surface as a confusing permission problem months later instead of a startup
// refusal.
func ResolveTopologyWithAdmin(ownerDSN, runtimeDSN, adminDSN string) (*Topology, error) {
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
	admin := ""
	resolvedAdminDSN := ""
	if adminDSN != "" {
		admin, err = roleFromDSN(adminDSN)
		if err != nil {
			return nil, fmt.Errorf("admin dsn: %w", err)
		}
		resolvedAdminDSN = adminDSN
	} else {
		admin = adminRoleName(runtimeRole)
		resolvedAdminDSN, err = withRole(runtimeDSN, admin)
		if err != nil {
			return nil, err
		}
	}
	if !validRole.MatchString(admin) {
		return nil, fmt.Errorf("admin role name %q is not a supported identifier", admin)
	}
	if strings.EqualFold(admin, ownerRole) {
		return nil, fmt.Errorf(
			"administrative role %q must differ from the schema owner %q: the "+
				"owner credential must never serve the organization-level path, "+
				"and an admin that is the owner can grant itself anything",
			admin, ownerRole)
	}
	if strings.EqualFold(admin, runtimeRole) {
		return nil, fmt.Errorf(
			"administrative role %q must differ from the application runtime role %q: "+
				"the audit chain and founder operations run on a principal that "+
				"workspace policies deliberately do not name",
			admin, runtimeRole)
	}
	return &Topology{
		OwnerDSN:       ownerDSN,
		RuntimeDSN:     runtimeDSN,
		AdminDSN:       resolvedAdminDSN,
		Runtime:        runtimeRole,
		Admin:          admin,
		PreProvisioned: preProvisionedRolesEnabled(),
	}, nil
}

// preProvisionedRolesEnabled reports whether the process must treat the
// runtime and admin roles as provisioned outside the application
// (AUSTRO_POSTGRES_PREPROVISIONED_ROLES). An unparseable value counts as off
// here because the strict configuration loader is the fail-fast gate that
// rejects such a value before the topology is ever resolved.
func preProvisionedRolesEnabled() bool {
	v, err := strconv.ParseBool(os.Getenv("AUSTRO_POSTGRES_PREPROVISIONED_ROLES"))
	return err == nil && v
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
// This is the SELF-PROVISIONED path: the process is the authority that creates
// the roles and sets their passwords. The pre-provisioned counterpart is
// assertPreProvisionedRoles, which must never create a role, alter a role or
// touch a password at all.
//
// The privileges are deliberately re-asserted in full each time: a grant made
// once would not cover a table created by a later migration, and a silent gap
// there surfaces as a confusing "permission denied" in production rather than
// as a security problem, which makes it easy to "fix" by reaching for the owner
// connection.
func provisionRoles(db *sql.DB, topo *Topology) error {
	for _, role := range []string{topo.Runtime, topo.Admin} {
		if !validRole.MatchString(role) {
			return fmt.Errorf("role name %q is not a supported identifier", role)
		}
	}

	// NOSUPERUSER / NOBYPASSRLS are asserted on every boot rather than only at
	// creation. A role altered out-of-band by an operator (or by a migration
	// tool) is corrected here, and if it cannot be, the runtime verification
	// that follows refuses to serve.
	for _, role := range []string{topo.Runtime, topo.Admin} {
		// #nosec G201 -- role identifiers are validated before DDL construction.
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
			// #nosec G201 -- role is validated and password is SQL-literal escaped.
			stmt := fmt.Sprintf("ALTER ROLE %s LOGIN PASSWORD %s", pair.role, quoteLit(pw))
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("set password for role %s: %w", pair.role, err)
			}
		}
	}

	return grantRolePrivileges(db, topo)
}

// assertPreProvisionedRoles verifies, against the live database, that the
// externally provisioned runtime and admin principals exist and carry exactly
// the attributes the security contract requires:
//
//   - the role exists
//   - the role is a LOGIN role
//   - the role is not a superuser
//   - the role does not hold BYPASSRLS
//   - the role does not hold CREATEROLE or CREATEDB
//
// It is the pre-provisioned counterpart of provisionRoles. Where that function
// would create the roles and set their passwords, this one refuses to serve on
// any deviation: the application has neither the right nor the need to repair
// a role that belongs to the platform, and a startup refusal is the only
// honest outcome for a topology that is not what the operator declared.
//
// The grant re-assertion that follows it (grantRolePrivileges) still runs in
// pre-provisioned mode: existing roles still need the schema privileges, and
// a migration that added a table must extend them. Granting is not role
// mutation — it touches no role attribute and no password.
func assertPreProvisionedRoles(db *sql.DB, topo *Topology) error {
	for _, role := range []string{topo.Runtime, topo.Admin} {
		if !validRole.MatchString(role) {
			return fmt.Errorf("role name %q is not a supported identifier", role)
		}
		if err := verifyPreProvisionedRole(db, role); err != nil {
			return err
		}
	}
	return nil
}

// verifyPreProvisionedRole checks one role in pg_roles. Every checked
// attribute is observable from any connection, so the check is trustworthy
// from the owner connection.
func verifyPreProvisionedRole(db *sql.DB, role string) error {
	var (
		superuser, login, bypass, createrole, createdb bool
	)
	err := db.QueryRow(`
		SELECT rolsuper, rolcanlogin, rolbypassrls, rolcreaterole, rolcreatedb
		FROM pg_roles WHERE rolname = $1`, role).
		Scan(&superuser, &login, &bypass, &createrole, &createdb)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"pre-provisioned role %q does not exist on the server; create it with "+
				"LOGIN, NOSUPERUSER, NOBYPASSRLS, NOCREATEDB and NOCREATEROLE before "+
				"starting with AUSTRO_POSTGRES_PREPROVISIONED_ROLES", role)
	}
	if err != nil {
		return fmt.Errorf("verify pre-provisioned role %s: %w", role, err)
	}
	if !login {
		return fmt.Errorf(
			"pre-provisioned role %q is not a LOGIN role and cannot authenticate", role)
	}
	if superuser {
		return fmt.Errorf(
			"pre-provisioned role %q is a superuser, which bypasses row level security", role)
	}
	if bypass {
		return fmt.Errorf(
			"pre-provisioned role %q holds BYPASSRLS, which bypasses row level security", role)
	}
	if createrole || createdb {
		var privs []string
		if createrole {
			privs = append(privs, "CREATEROLE")
		}
		if createdb {
			privs = append(privs, "CREATEDB")
		}
		return fmt.Errorf(
			"pre-provisioned role %q holds %s, which the security contract forbids",
			role, strings.Join(privs, " and "))
	}
	return nil
}

// grantRolePrivileges grants the runtime and admin principals exactly the
// privileges the application needs. Both modes run it: self-provisioned after
// creating the roles, pre-provisioned after verifying them.
//
// Runtime role: ordinary workspace-scoped DML on every table, then the audit
// table narrowed to append-only. Revoking UPDATE and DELETE there is what
// makes the audit log append-only from the application's own connection,
// independent of any application-level discipline.
// #nosec G201 -- all interpolated role identifiers are validated by the
// callers before this function is reached.
func grantRolePrivileges(db *sql.DB, topo *Topology) error {
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
		// #nosec G201 -- table is fixed and ownerRole is validated above.
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
		// #nosec G201 -- table is from the fixed rlsTables allowlist.
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", table)); err != nil {
			return fmt.Errorf("force row level security on %s: %w", table, err)
		}
	}
	return nil
}
