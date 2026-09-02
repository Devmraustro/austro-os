// Package authz provides a deny-by-default authorization enforcement point.
//
// The invariant is strict: if no explicit allow rule grants a request, the
// request is DENIED. There is no permissive fallback and no default-allow.
// An endpoint added without a matching rule can never become public implicitly.
package authz

import (
	"errors"
	"net/http"

	"austro-os/internal/auth"
)

// ErrDenied reports a request that was not explicitly granted.
var ErrDenied = errors.New("access denied")

// permissionKey is the canonical "action:resource" key for explicit rules.
type permissionKey struct {
	Action   string
	Resource string
}

// Authorizer enforces explicit, deny-by-default permissions over auth claims.
type Authorizer struct {
	// rules maps an action+resource to the workspaces its holders are scoped
	// to. An empty workspace list means the rule applies to any workspace.
	rules map[permissionKey][]string
}

// NewAuthorizer returns an empty deny-by-default authorizer. Grant layers seed
// explicit allow rules via AddRule.
func NewAuthorizer() *Authorizer {
	return &Authorizer{rules: map[permissionKey][]string{}}
}

// AddRule declares an explicit allow rule: holders of the permission for
// action+resource may act within the listed workspaces. Passing no workspaces
// allows any workspace.
func (a *Authorizer) AddRule(action, resource string, workspaces ...string) {
	a.rules[permissionKey{Action: action, Resource: resource}] = workspaces
}

// Authorize decides whether an HTTP request is explicitly permitted. It is the
// single enforcement point wired into the request path. A request is allowed
// ONLY when an explicit rule matches the action+resource AND the caller is
// authenticated AND the claims carry the permission AND workspace scope
// matches. Everything else is denied.
func (a *Authorizer) Authorize(r *http.Request, action, resource string) error {
	claims, ok := auth.FromRequest(r)
	if !ok {
		return ErrDenied
	}
	return a.AuthorizeClaims(claims, action, resource)
}

// AuthorizeClaims grants a request only if an explicit rule for the
// action+resource exists, the claims carry the permission, and the workspace
// scope matches. No explicit rule means DENY.
func (a *Authorizer) AuthorizeClaims(claims *auth.Claims, action, resource string) error {
	if claims == nil {
		return ErrDenied
	}
	allowed, present := a.rules[permissionKey{Action: action, Resource: resource}]
	if !present {
		// No explicit rule -> DENY (no permissive fallback).
		return ErrDenied
	}
	if !hasPermission(claims, action, resource) {
		return ErrDenied
	}
	if !hasScope(allowed, claims.WorkspaceID) {
		return ErrDenied
	}
	return nil
}

// hasPermission reports whether the claims explicitly grant the action for the
// resource.
func hasPermission(claims *auth.Claims, action, resource string) bool {
	perms, ok := claims.Permissions[action]
	if !ok {
		return false
	}
	for _, res := range perms {
		if res == resource {
			return true
		}
	}
	return false
}

// hasScope reports whether the claims' workspace is covered by the rule's
// allowed workspaces. A rule with no workspace list covers any workspace.
func hasScope(allowed []string, workspace string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, ws := range allowed {
		if ws == workspace {
			return true
		}
	}
	return false
}