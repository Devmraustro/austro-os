package api

import "net/http"

// Route identifies one HTTP route this server exposes: the method and the
// net/http ServeMux pattern.
type Route struct {
	Method  string
	Pattern string
}

// String renders the route in the "METHOD /pattern" form used by ServeMux and
// by the OpenAPI document.
func (r Route) String() string { return r.Method + " " + r.Pattern }

// Routes is the canonical list of routes this server exposes, and the single
// source of truth for the HTTP surface.
//
// Three things are pinned to it, so the surface cannot silently drift:
//
//   - main.go registers exactly these routes. A handler with no entry here, or
//     an entry with no handler, is a fatal startup error rather than a route
//     that exists in one place and not the other.
//   - api/openapi.yaml must describe exactly these operations. The parity test
//     fails when the document advertises an operation the server does not
//     serve, or omits one it does.
//   - Authorization stays deny-by-default: a route listed here is still refused
//     unless internal/rbac grants an explicit allow rule for it, or
//     isPublicEndpoint exempts it by contract.
//
// Adding a route therefore means adding it here, registering a handler, granting
// an authorization rule where it is not public, and documenting it — and the
// build fails at whichever step is skipped.
func Routes() []Route {
	return []Route{
		{http.MethodGet, "/health/live"},
		{http.MethodGet, "/health/ready"},

		{http.MethodPost, "/api/auth/bootstrap"},
		{http.MethodPost, "/api/auth/login"},
		{http.MethodPost, "/api/auth/refresh"},
		{http.MethodPost, "/api/auth/logout"},
		{http.MethodGet, "/api/me"},

		{http.MethodGet, "/workspaces"},
		{http.MethodPost, "/workspaces"},
		{http.MethodGet, "/workspaces/{id}"},

		// Audit visibility. Read-only by construction: no audit route accepts a
		// body, and the chain stays append-only behind the audit store.
		//
		// /audit/events and /audit/verification are organization-scoped and
		// founder-only; they are served from the administrative handle, which is
		// the only principal audit_org_policy names.
		{http.MethodGet, "/audit/events"},
		{http.MethodGet, "/audit/verification"},
		// The workspace-scoped read is served from the unprivileged runtime
		// handle with the workspace bound in PostgreSQL, so tenant isolation is
		// enforced by the database rather than by the handler.
		{http.MethodGet, "/workspaces/{id}/audit/events"},
	}
}
