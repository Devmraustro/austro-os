// Package authz provides a deny-by-default authorization enforcement point for
// the workspace-scoped RBAC model (internal/rbac).
//
// The invariant is strict: if no explicit allow rule grants a request, the
// request is DENIED. There is no permissive fallback and no default-allow.
// An endpoint added without a matching rule can never become public implicitly.
//
// A request is granted ONLY when ALL of the following hold:
//   - the caller is authenticated (verified claims are present and shaped
//     correctly: a valid role and a consistent role/workspace pairing);
//   - an explicit rule matches the HTTP method and the route (exact or a
//     single {id} template segment);
//   - the rule allows the caller's role;
//   - the caller's claims carry the permission for the action+route;
//   - the rule's workspace scope is satisfied, and for ScopePath the workspace
//     supplied in the path equals the workspace in the claims (the token's
//     workspace is authoritative; a client can never select a workspace by
//     rewriting a URL, header, body field or query parameter).
package authz

import (
	"errors"
	"net/http"
	"strings"

	"austro-os/internal/auth"
	"austro-os/internal/rbac"
)

// ErrDenied reports a request that was not explicitly granted.
var ErrDenied = errors.New("access denied")

// permissionKey is the canonical "action:resource pattern" for explicit rules.
type permissionKey struct {
	Action   string
	Resource string
}

// rule is one explicit allow contract for an action+resource pattern.
type rule struct {
	roles []rbac.Role
	scope rbac.WorkspaceScope
}

// Authorizer enforces explicit, deny-by-default, workspace-scoped permissions
// over verified auth claims. All mutation methods are called at startup from a
// reviewable seed list; the request path is read-only.
type Authorizer struct {
	rules []ruleEntry
}

type ruleEntry struct {
	key  permissionKey
	rule rule
}

// NewAuthorizer returns an empty deny-by-default authorizer. Grant layers seed
// explicit allow rules via AddRule or AddRules.
func NewAuthorizer() *Authorizer {
	return &Authorizer{}
}

// AddRule declares an explicit allow rule: principals holding role r may act
// on action+resource under the given workspace scope. The resource may contain
// a single {id} segment that captures the target workspace under ScopePath.
func (a *Authorizer) AddRule(action, resource string, scope rbac.WorkspaceScope, roles ...rbac.Role) {
	a.rules = append(a.rules, ruleEntry{
		key:  permissionKey{Action: action, Resource: resource},
		rule: rule{roles: roles, scope: scope},
	})
}

// AddRules seeds the authorizer from the declared RBAC contract. Only the
// rules for routes actually registered by the server should be seeded.
func (a *Authorizer) AddRules(rs []rbac.Rule) {
	for _, r := range rs {
		a.AddRule(r.Action, r.ResourcePattern, r.Scope, r.Roles...)
	}
}

// Authorize decides whether an HTTP request is explicitly permitted. It is the
// single enforcement point wired into the request path.
func (a *Authorizer) Authorize(r *http.Request, action, resource string) error {
	claims, ok := auth.FromRequest(r)
	if !ok {
		return ErrDenied
	}
	return a.AuthorizeClaims(claims, action, resource)
}

// AuthorizeClaims grants a request only if an explicit rule for the
// action+resource exists, the claims are shaped correctly (valid role, valid
// role/workspace pairing), the rule allows the role, the claims carry the
// permission, and the workspace scope matches. No explicit rule means DENY.
func (a *Authorizer) AuthorizeClaims(claims *auth.Claims, action, resource string) error {
	if claims == nil {
		return ErrDenied
	}
	if !rbac.ValidRole(claims.Role) {
		// A token carrying an unsupported role is forged or corrupt: deny.
		return ErrDenied
	}
	role := claims.Role

	for _, e := range a.rules {
		if e.key.Action != action {
			continue
		}
		captured, matched := matchResource(e.key.Resource, resource)
		if !matched {
			continue
		}
		rule := e.rule
		if !hasRole(rule.roles, role) {
			// Role not allowed for this route -> role denial.
			continue
		}
		if !hasPermission(claims, action, resource) {
			// Claims do not carry the permission for the action+route.
			continue
		}
		if !inScope(rule.scope, captured, claims.WorkspaceID) {
			continue
		}
		return nil
	}
	return ErrDenied
}

// hasRole reports whether the rule allows the role.
func hasRole(roles []rbac.Role, role rbac.Role) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// hasPermission reports whether the claims explicitly grant the action for the
// concrete resource. Grants are minted with template resources (e.g.
// /workspaces/{id}), so each granted resource is matched as a template.
func hasPermission(claims *auth.Claims, action, resource string) bool {
	perms, ok := claims.Permissions[action]
	if !ok {
		return false
	}
	for _, granted := range perms {
		if _, matched := matchResource(granted, resource); matched {
			return true
		}
	}
	return false
}

// inScope reports whether the rule's workspace binding is satisfied by the
// claims' workspace and the captured path workspace. The authoritative
// workspace is claims.WorkspaceID (minted from the database at login); a
// client-supplied path workspace only satisfies ScopePath when it equals it.
func inScope(scope rbac.WorkspaceScope, captured, claimsWorkspace string) bool {
	switch scope {
	case rbac.ScopeNone:
		return true
	case rbac.ScopeSelf:
		return claimsWorkspace != ""
	case rbac.ScopePath:
		return claimsWorkspace != "" && captured != "" && captured == claimsWorkspace
	}
	return false
}

// matchResource matches a template resource against a concrete request path.
// Any non-empty `{name}` segment is a single path-segment placeholder. This
// keeps the existing {id} routes strict while allowing key-based resources
// such as /memory/{layer}/{key} without treating a slash as part of a key.
// The first captured segment is returned for ScopePath rules; Memory uses
// ScopeSelf, so its layer/key placeholders never become a workspace binding.
func matchResource(pattern, actual string) (string, bool) {
	pSegs := splitPath(pattern)
	aSegs := splitPath(actual)
	if len(pSegs) != len(aSegs) {
		return "", false
	}
	var captured string
	for i := range pSegs {
		segment := pSegs[i]
		placeholder := len(segment) > 2 && strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}")
		if placeholder {
			if aSegs[i] == "" {
				return "", false
			}
			if captured == "" {
				captured = aSegs[i]
			}
			continue
		}
		if segment != aSegs[i] {
			return "", false
		}
	}
	return captured, true
}

// splitPath splits a route or path into its non-empty segments, preserving
// casing.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
