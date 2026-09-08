// Package rbac defines the explicit, workspace-scoped role model for AUSTRO OS.
//
// The model distinguishes exactly three roles:
//
//   - founder           organization-level authority (the single founder identity)
//   - workspace_admin   administrative authority within one workspace
//   - workspace_member  member/user authority within one workspace
//
// Authorization is deny-by-default, workspace-scoped, explicit, deterministic
// and auditable at the enforcement point (internal/authz). Roles never come
// from the client: they are read from the persisted identity (users.role) at
// token-issuance time, carried in verified JWT claims, and re-validated on
// every authorization decision. A role that is not one of the three constants
// is unsupported and denies (see authz.AuthorizeClaims).
//
// The declared rule set (Rules) is the protection contract. Only rules that
// correspond to a route actually registered by the server are seeded into the
// runtime authorizer; every other route is left unregistered and is denied by
// the deny-by-default enforcement point. No permissive fallback exists.
//
// Workspace scope is expressed per rule (WorkspaceScope):
//
//   - ScopeNone: organization-level, no database workspace binding is required
//     (the caller's own profile, or an explicit founder org-level authority).
//   - ScopeSelf: the caller is bound to the workspace in its verified claims.
//   - ScopePath: the caller must supply a workspace in the route path that MUST
//     equal the workspace in its verified claims. A mismatched or missing path
//     workspace is denied: the authoritative workspace is the one from the
//     token (and therefore the database), never one the client happens to send.
package rbac

// Role is the authorization role of an identity. Exactly three roles exist;
// ValidRole is the single source of truth for what is supported.
type Role string

const (
	// RoleFounder is the single organization-level identity. It is the only
	// role allowed a workspace-less (cross-workspace) authority, and only
	// through explicit rules — never through an implicit fallback.
	RoleFounder Role = "founder"
	// RoleWorkspaceAdmin holds administrative authority within its workspace.
	RoleWorkspaceAdmin Role = "workspace_admin"
	// RoleWorkspaceMember holds member/user authority within its workspace.
	RoleWorkspaceMember Role = "workspace_member"
)

// ValidRole reports whether r is one of the supported roles.
func ValidRole(r Role) bool {
	switch r {
	case RoleFounder, RoleWorkspaceAdmin, RoleWorkspaceMember:
		return true
	}
	return false
}

// WorkspaceScope states how a rule binds the caller to a workspace.
type WorkspaceScope int

const (
	// ScopeNone does not bind the caller to a database workspace: the operation
	// is organization-level (own profile) or an explicit founder org-level
	// authority.
	ScopeNone WorkspaceScope = iota
	// ScopeSelf binds the caller to the workspace in its verified claims. A
	// claim set without a workspace is not a valid member/admin identity and
	// is denied.
	ScopeSelf
	// ScopePath requires a workspace in the route path that equals the
	// caller's verified claims workspace. A client cannot switch workspaces by
	// editing the path, a header, a JSON field or a query parameter.
	ScopePath
)

func (s WorkspaceScope) String() string {
	switch s {
	case ScopeNone:
		return "none"
	case ScopeSelf:
		return "self"
	case ScopePath:
		return "path"
	}
	return "unknown"
}

// Rule is one explicit authorization contract: who (roles) may act (action) on
// a route (ResourcePattern) under which workspace binding (Scope). A route may
// have several rules when roles differ in scope (for example a founder's
// organization-level read and a workspace admin's own-workspace read).
type Rule struct {
	// Action is the HTTP method.
	Action string
	// ResourcePattern is the route, which may contain a single {id} segment
	// that captures the target workspace when Scope is ScopePath.
	ResourcePattern string
	// Roles are the roles allowed by this contract (non-empty).
	Roles []Role
	// Scope is the workspace binding this contract requires.
	Scope WorkspaceScope
	// Principle names the constitutional principle that governs the endpoint.
	Principle string
}

// Rules returns the declared protection surface. It is the documented RBAC
// model: profile reads for every authenticated principal, and workspace-scoped
// administration for founder and workspace admin. Only the routes registered
// by the server are seeded into the runtime authorizer; the rest are declared
// for the later Phase 3 steps that implement them, and remain denied until an
// explicit rule plus a registered route both exist.
func Rules() []Rule {
	return []Rule{
		// /api/me: an authenticated principal's own safe profile. No database
		// workspace is accessed, so no binding is required.
		{
			Action:          "GET",
			ResourcePattern: "/api/me",
			Roles:           []Role{RoleFounder, RoleWorkspaceAdmin, RoleWorkspaceMember},
			Scope:           ScopeNone,
			Principle:       "Principle 9 - Security by Design",
		},
		// /workspaces: founder organization-level authority (org listing and
		// workspace creation). A workspace is not tenant data contained inside
		// another workspace, so creating one is an organization-level operation
		// only the founder may perform; a workspace admin must never enumerate
		// or create workspaces organization-wide.
		{
			Action:          "GET",
			ResourcePattern: "/workspaces",
			Roles:           []Role{RoleFounder},
			Scope:           ScopeNone,
			Principle:       "Principle 9 - Security by Design",
		},
		{
			Action:          "POST",
			ResourcePattern: "/workspaces",
			Roles:           []Role{RoleFounder},
			Scope:           ScopeNone,
			Principle:       "Principle 5 - Modular Design",
		},
		// /workspaces/{id}: workspace-scoped administration read. A workspace
		// admin may read only its own workspace; the founder's org-level
		// authority is an explicit rule, not an accidental bypass.
		{
			Action:          "GET",
			ResourcePattern: "/workspaces/{id}",
			Roles:           []Role{RoleFounder},
			Scope:           ScopeNone,
			Principle:       "Principle 10 - Privacy by Design",
		},
		{
			Action:          "GET",
			ResourcePattern: "/workspaces/{id}",
			Roles:           []Role{RoleWorkspaceAdmin},
			Scope:           ScopePath,
			Principle:       "Principle 10 - Privacy by Design",
		},
	}
}

// ImplementedRules returns the rules for routes actually registered by the
// HTTP server. It is the only rule set seeded into the runtime authorizer;
// anything else is denied because it has no rule and no route. The workspace
// administration surface (GET/POST /workspaces, GET /workspaces/{id}) is
// registered by the server, so its rules are seeded here.
func ImplementedRules() []Rule {
	return []Rule{
		{
			Action:          "GET",
			ResourcePattern: "/api/me",
			Roles:           []Role{RoleFounder, RoleWorkspaceAdmin, RoleWorkspaceMember},
			Scope:           ScopeNone,
			Principle:       "Principle 9 - Security by Design",
		},
		{
			Action:          "GET",
			ResourcePattern: "/workspaces",
			Roles:           []Role{RoleFounder},
			Scope:           ScopeNone,
			Principle:       "Principle 9 - Security by Design",
		},
		{
			Action:          "POST",
			ResourcePattern: "/workspaces",
			Roles:           []Role{RoleFounder},
			Scope:           ScopeNone,
			Principle:       "Principle 5 - Modular Design",
		},
		{
			Action:          "GET",
			ResourcePattern: "/workspaces/{id}",
			Roles:           []Role{RoleFounder},
			Scope:           ScopeNone,
			Principle:       "Principle 10 - Privacy by Design",
		},
		{
			Action:          "GET",
			ResourcePattern: "/workspaces/{id}",
			Roles:           []Role{RoleWorkspaceAdmin},
			Scope:           ScopePath,
			Principle:       "Principle 10 - Privacy by Design",
		},
	}
}

// PermissionsForRole returns the explicit permissions (HTTP action -> route
// resources) an access token carries for a role. Only these grants are minted
// into claims; everything else stays absent and therefore denied. Grants use
// the same {id} templates as the rules so an authorization decision can match a
// concrete request path against the token grant.
func PermissionsForRole(r Role) map[string][]string {
	switch r {
	case RoleFounder:
		return map[string][]string{
			"GET":  {"/api/me", "/workspaces", "/workspaces/{id}"},
			"POST": {"/workspaces"},
		}
	case RoleWorkspaceAdmin:
		return map[string][]string{
			"GET": {"/api/me", "/workspaces/{id}"},
		}
	case RoleWorkspaceMember:
		return map[string][]string{
			"GET": {"/api/me"},
		}
	default:
		return nil
	}
}
