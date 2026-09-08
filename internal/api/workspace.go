package api

import (
	"errors"

	"austro-os/internal/auth"
	"austro-os/internal/rbac"
)

// errNoWorkspace reports a claims set that cannot be bound to a database
// workspace for a workspace-scoped operation.
var errNoWorkspace = errors.New("no workspace bound to identity")

// WorkspaceFromClaims returns the database workspace context to use for a
// workspace-scoped operation, derived exclusively from the verified claims of
// the authenticated caller.
//
// This is the only bridge between HTTP authorization and the workspace-scoped
// data stores: handlers MUST use this value (never a workspace the client
// supplied in a URL, header, body, or query parameter) as the workspace
// argument of any store operation, so that RLS's app.current_workspace binding
// and the explicit WHERE workspace_id guards agree with the token, not with
// the client.
//
//   - founder: identifiers have no home workspace. A founder is granted
//     organization-level reads only through explicit ScopeNone rules; no
//     workspace is derived here, so a founder cannot silently become a
//     cross-workspace reader of tenant data.
//   - workspace_admin / workspace_member: an identity bound to a workspace in
//     the database; that workspace is returned. A member/admin claim with no
//     workspace cannot be minted by the system (VerifyAccessToken rejects it)
//     and is refused here as defense-in-depth.
func WorkspaceFromClaims(claims *auth.Claims) (string, error) {
	if claims == nil {
		return "", errNoWorkspace
	}
	switch claims.Role {
	case rbac.RoleFounder:
		return "", errNoWorkspace
	case rbac.RoleWorkspaceAdmin, rbac.RoleWorkspaceMember:
		if claims.WorkspaceID == "" {
			return "", errNoWorkspace
		}
		return claims.WorkspaceID, nil
	default:
		return "", errNoWorkspace
	}
}
