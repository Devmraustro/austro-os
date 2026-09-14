package api

import (
	"net/http"
	"strconv"
	"strings"

	"austro-os/internal/audit"
	"austro-os/internal/auth"
	logger "austro-os/internal/log"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
)

// AuditHandler exposes the persistent audit chain for reading.
//
//   - GET /audit/events                     organization-scoped page (founder)
//   - GET /workspaces/{id}/audit/events     workspace-scoped page
//   - GET /audit/verification               whole-chain integrity check (founder)
//
// There is deliberately no mutation path. The chain is append-only and is
// written by the audit store from inside the flows that produce events; nothing
// here can update, delete or reorder a row, and no route in this file accepts a
// request body.
//
// Two readers with different privileges back these routes, and the split is the
// security model:
//
//	org      -- the administrative handle, named by audit_org_policy, which is
//	          the only thing that makes an organization-wide view possible.
//	          Reachable only by the founder.
//	tenant   -- the unprivileged runtime handle, confined by
//	          audit_workspace_policy through a bound app.current_workspace, so
//	          isolation is enforced by PostgreSQL and not by this file.
type AuditHandler struct {
	org    audit.Reader
	tenant audit.Reader
}

// NewAuditHandler builds the handler from both readers. Both are required: a
// nil tenant reader would mean workspace-scoped reads silently fall back to the
// organization-wide one, which is precisely the privilege escalation this
// separation exists to prevent.
func NewAuditHandler(org, tenant audit.Reader) *AuditHandler {
	return &AuditHandler{org: org, tenant: tenant}
}

// auditPage is the response envelope. next_cursor is the seq to pass back as
// before_seq for the following page, and is omitted when the page was short
// enough to be the last one.
type auditPage struct {
	Events     []audit.EventView `json:"events"`
	NextCursor string            `json:"next_cursor,omitempty"`
	Limit      int               `json:"limit"`
}

// ListOrg returns an organization-wide audit page. Founder only.
func (h *AuditHandler) ListOrg(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if claims.Role != rbac.RoleFounder {
		auditWorkspace(claims.ID, "list-audit-org", "denied", "role_insufficient")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	q, ok := parseAuditQuery(w, r)
	if !ok {
		return
	}
	if q.Limit <= 0 {
		q.Limit = audit.DefaultPageSize
	}
	events, err := h.org.List(r.Context(), q)
	if err != nil {
		h.serverError(w, "list-audit-org", err)
		return
	}
	writeJSON(w, http.StatusOK, newAuditPage(events, q.Limit))
}

// ListForWorkspace returns one workspace's audit page.
//
// The founder may name any workspace and is served from the organization
// reader with an explicit filter. A workspace admin or member is served from
// the tenant reader with the workspace bound in PostgreSQL, and the workspace
// comes from the verified claims -- never from the path and never from a query
// parameter. The path id is checked against the claims as defense-in-depth even
// though the authorization middleware already enforced it.
func (h *AuditHandler) ListForWorkspace(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q, ok := parseAuditQuery(w, r)
	if !ok {
		return
	}
	if q.Limit <= 0 {
		q.Limit = audit.DefaultPageSize
	}

	pathID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid workspace id")
		return
	}

	if claims.Role == rbac.RoleFounder {
		q.Workspace = &pathID
		events, err := h.org.List(r.Context(), q)
		if err != nil {
			h.serverError(w, "list-audit-workspace", err)
			return
		}
		writeJSON(w, http.StatusOK, newAuditPage(events, q.Limit))
		return
	}

	// Non-founder roles are confined to their own workspace. Taking the
	// binding from the claims rather than the path means a forged or mistyped
	// path cannot redirect the read, and a diverging path is refused outright
	// rather than quietly ignored.
	binding, err := WorkspaceFromClaims(claims)
	if err != nil {
		auditWorkspace(claims.ID, "list-audit-workspace", "denied", "no_workspace_context")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if binding != pathID.String() {
		auditWorkspace(claims.ID, "list-audit-workspace", "denied", "workspace_mismatch")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	events, err := h.tenant.ListForWorkspace(r.Context(), pathID, q)
	if err != nil {
		h.serverError(w, "list-audit-workspace", err)
		return
	}
	writeJSON(w, http.StatusOK, newAuditPage(events, q.Limit))
}

// auditVerification is the integrity report. Verified is the whole-chain hash
// check; Length is how many events it covered.
type auditVerification struct {
	Verified bool  `json:"verified"`
	Length   int   `json:"events_checked"`
	HeadSeq  int64 `json:"head_seq,omitempty"`
}

// Verification re-reads the persisted chain and recomputes every link.
//
// This is the expensive route and it says so: Verify walks the entire table, so
// cost grows with history and there is no bounded version of a whole-chain
// proof. It is founder-only and is meant to be run deliberately, not polled.
func (h *AuditHandler) Verification(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if claims.Role != rbac.RoleFounder {
		auditWorkspace(claims.ID, "verify-audit", "denied", "role_insufficient")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	// Verify is exposed through the reader's store because it needs the full
	// chain, which List deliberately never returns.
	ok2, length, err := h.org.VerifyChain(r.Context())
	if err != nil {
		h.serverError(w, "verify-audit", err)
		return
	}
	head := int64(0)
	if length > 0 {
		// The newest event is the head; one bounded read gets its seq.
		if page, err := h.org.List(r.Context(), audit.Query{Limit: 1}); err == nil && len(page) > 0 {
			head = page[0].Seq
		}
	}
	writeJSON(w, http.StatusOK, auditVerification{Verified: ok2, Length: length, HeadSeq: head})
}

// parseAuditQuery reads the bounded filter set from the query string. Unknown
// parameters are rejected rather than ignored: a caller who typos a filter
// should get an error, not an unfiltered page that looks like it worked.
func parseAuditQuery(w http.ResponseWriter, r *http.Request) (audit.Query, bool) {
	var q audit.Query
	known := map[string]bool{
		"event_type": true, "outcome": true, "actor_type": true,
		"limit": true, "before_seq": true,
	}
	values := r.URL.Query()
	for key := range values {
		if !known[key] {
			writeJSONError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
			return q, false
		}
	}
	q.EventType = strings.TrimSpace(values.Get("event_type"))
	q.Outcome = strings.TrimSpace(values.Get("outcome"))
	q.ActorType = strings.TrimSpace(values.Get("actor_type"))

	if v := values.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return q, false
		}
		q.Limit = n
	}
	if v := values.Get("before_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "before_seq must be a non-negative integer")
			return q, false
		}
		q.BeforeSeq = n
	}
	return q, true
}

// newAuditPage builds the envelope. The cursor is offered only when the page
// came back full: a short page means the end of the history, and advertising a
// cursor there would send the client on a request that can only return empty.
func newAuditPage(events []audit.EventView, limit int) auditPage {
	page := auditPage{Events: events, Limit: limit}
	if len(events) == limit && limit > 0 {
		page.NextCursor = strconv.FormatInt(events[len(events)-1].Seq, 10)
	}
	return page
}

func (h *AuditHandler) serverError(w http.ResponseWriter, action string, err error) {
	logger.NewEntry("audit-handler-error").SetLevel("error").
		With("action", action).WithError(err).Log()
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}
