package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"austro-os/internal/audit"
	"austro-os/internal/auth"
	"austro-os/internal/knowledge"
	logger "austro-os/internal/log"
	"austro-os/internal/middleware"

	"github.com/google/uuid"
)

// KnowledgeHandler exposes the knowledge contract finalized in ADR-007 and
// completed by ADR-023:
//
//	POST   /knowledge            create a document in the caller's workspace
//	GET    /knowledge            bounded, newest-first listing
//	GET    /knowledge/{id}       read one document
//	PATCH  /knowledge/{id}       update title, content or kind
//	DELETE /knowledge/{id}       remove a document
//	POST   /knowledge/search     top-k by embedding similarity
//
// Two properties are load-bearing and shape every method below.
//
// The workspace comes only from the verified token, via WorkspaceFromClaims. No
// query parameter, header or body field selects or widens it, so a client cannot
// reach another tenant's corpus by asking for it.
//
// The embedding is never accepted from a client and never returned to one. It is
// server-derived from the content: a caller-supplied vector would let one
// workspace plant a document that another workspace's similarity search
// retrieves, which is precisely the isolation the column exists to serve. It is
// also storage mechanics, not part of the document a reader wants.
type KnowledgeHandler struct {
	svc       *knowledge.Service
	auditSink audit.Sink
}

// NewKnowledgeHandler returns a handler over the knowledge service.
func NewKnowledgeHandler(svc *knowledge.Service) *KnowledgeHandler {
	return &KnowledgeHandler{svc: svc}
}

// SetAuditSink attaches the persistent audit chain and returns the handler.
//
// Audit is recorded here rather than in the service for two structural reasons.
// knowledge.AuditSink.Record returns no error, so a service-level record cannot
// fail closed and a dropped audit entry would be invisible; auditstore.Append
// does return an error. And the service hardcodes ActorType "system", while only
// the handler holds the verified claims that name the person who acted.
func (h *KnowledgeHandler) SetAuditSink(sink audit.Sink) *KnowledgeHandler {
	h.auditSink = sink
	return h
}

// createKnowledgeRequest is the POST body. Kind is optional and defaults to
// "document": an optional field that the domain rejects when omitted is not
// optional, which was a real defect in the task contract.
type createKnowledgeRequest struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// updateKnowledgeRequest is the PATCH body. Every field is a pointer so that
// "absent" is distinguishable from "set to the empty string": a PATCH that omits
// the title must leave the title alone.
type updateKnowledgeRequest struct {
	Kind    *string `json:"kind"`
	Title   *string `json:"title"`
	Content *string `json:"content"`
}

// searchKnowledgeRequest is the POST body for a similarity search. The query is
// embedded server-side; no vector is accepted.
type searchKnowledgeRequest struct {
	Query string `json:"query"`
	Kind  string `json:"kind"`
	// Limit is a pointer so that an omitted limit (use the default) is
	// distinguishable from an explicit 0 (a mistake). With a plain int both
	// arrive as 0, and silently defaulting one of them hides the other.
	Limit *int `json:"limit"`
}

// knowledgeResponse is one document as the API exposes it. The embedding is
// deliberately absent -- see the type comment on KnowledgeHandler.
type knowledgeResponse struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// knowledgePage is the listing envelope. next_cursor is omitted on the last page.
type knowledgePage struct {
	Documents  []knowledgeResponse `json:"documents"`
	NextCursor string              `json:"next_cursor,omitempty"`
	Limit      int                 `json:"limit"`
}

func newKnowledgeResponse(d *knowledge.Document) knowledgeResponse {
	return knowledgeResponse{
		ID:          d.ID.String(),
		WorkspaceID: d.WorkspaceID.String(),
		Kind:        string(d.Kind),
		Title:       d.Title,
		Content:     d.Content,
		CreatedAt:   d.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   d.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// scope resolves the caller and the workspace it may act in. Both come from the
// verified token; a founder has no workspace and is refused rather than widened
// to every tenant.
func (h *KnowledgeHandler) scope(w http.ResponseWriter, r *http.Request, action string) (*auth.Claims, uuid.UUID, bool) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, uuid.Nil, false
	}
	binding, err := WorkspaceFromClaims(claims)
	if err != nil {
		h.recordFailure(r, claims, action, uuid.Nil, uuid.Nil, "denied", "no_workspace_context")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	ws, err := uuid.Parse(binding)
	if err != nil {
		h.recordFailure(r, claims, action, uuid.Nil, uuid.Nil, "denied", "invalid_workspace_claim")
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	return claims, ws, true
}

// Create makes a new knowledge document in the caller's workspace.
func (h *KnowledgeHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "create")
	if !ok {
		return
	}
	var req createKnowledgeRequest
	if !decodeJSON(w, r, &req) {
		h.recordFailure(r, claims, "create", uuid.Nil, ws, "failed", "malformed_body")
		return
	}

	kind, ok := h.kind(w, r, claims, ws, req.Kind, "create", uuid.Nil, true)
	if !ok {
		return
	}
	if err := checkContentLength(w, r, claims, ws, "create", uuid.Nil, h, req.Title, req.Content); err != nil {
		return
	}

	doc, err := h.svc.Create(r.Context(), ws, kind, req.Title, req.Content)
	if err != nil {
		h.fail(w, r, claims, "create", uuid.Nil, ws, err)
		return
	}
	if !h.recordOrInternal(w, r, claims, "create", doc.ID, ws) {
		return
	}
	writeJSON(w, http.StatusCreated, newKnowledgeResponse(doc))
}

// List returns one bounded page of the caller's documents, newest first.
func (h *KnowledgeHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "list")
	if !ok {
		return
	}
	q, ok := h.parseQuery(w, r, claims, ws)
	if !ok {
		return
	}
	page, err := h.svc.ListPage(r.Context(), ws, q)
	if err != nil {
		h.fail(w, r, claims, "list", uuid.Nil, ws, err)
		return
	}
	out := knowledgePage{Documents: make([]knowledgeResponse, 0, len(page.Documents)), Limit: page.Limit}
	for _, d := range page.Documents {
		out.Documents = append(out.Documents, newKnowledgeResponse(d))
	}
	out.NextCursor = page.NextCursor
	writeJSON(w, http.StatusOK, out)
}

// Get returns one document in the caller's workspace.
func (h *KnowledgeHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "get")
	if !ok {
		return
	}
	id, ok := h.docID(w, r, claims, ws, "get")
	if !ok {
		return
	}
	doc, err := h.svc.Get(r.Context(), ws, id)
	if err != nil {
		h.fail(w, r, claims, "get", id, ws, err)
		return
	}
	writeJSON(w, http.StatusOK, newKnowledgeResponse(doc))
}

// Update modifies the mutable fields of a document. Content is re-embedded only
// when it actually changes, which the service decides.
func (h *KnowledgeHandler) Update(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "update")
	if !ok {
		return
	}
	id, ok := h.docID(w, r, claims, ws, "update")
	if !ok {
		return
	}
	var req updateKnowledgeRequest
	if !decodeJSON(w, r, &req) {
		h.recordFailure(r, claims, "update", id, ws, "failed", "malformed_body")
		return
	}
	if req.Kind == nil && req.Title == nil && req.Content == nil {
		// An empty PATCH is a client mistake, not a no-op: silently returning
		// 200 would tell the caller something was written when nothing was.
		writeJSONError(w, http.StatusBadRequest, "at least one field must be provided")
		h.recordFailure(r, claims, "update", id, ws, "failed", "empty_update")
		return
	}

	var kind *knowledge.Kind
	if req.Kind != nil {
		trimmed := strings.TrimSpace(*req.Kind)
		if trimmed == "" {
			writeJSONError(w, http.StatusBadRequest, "kind must not be blank")
			h.recordFailure(r, claims, "update", id, ws, "failed", "kind_blank")
			return
		}
		k, err := knowledge.ValidateKind(trimmed)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "kind must be document, campaign_rule or style_guide")
			h.recordFailure(r, claims, "update", id, ws, "failed", "invalid_kind")
			return
		}
		kind = &k
	}

	doc, err := h.svc.Update(r.Context(), ws, id, req.Title, req.Content, kind)
	if err != nil {
		h.fail(w, r, claims, "update", id, ws, err)
		return
	}
	if !h.recordOrInternal(w, r, claims, "update", doc.ID, ws) {
		return
	}
	writeJSON(w, http.StatusOK, newKnowledgeResponse(doc))
}

// Delete removes a document. The knowledge domain has no archive or status
// concept, so this is its only retirement path; the UI confirms before calling.
func (h *KnowledgeHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "delete")
	if !ok {
		return
	}
	id, ok := h.docID(w, r, claims, ws, "delete")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), ws, id); err != nil {
		h.fail(w, r, claims, "delete", id, ws, err)
		return
	}
	if !h.recordOrInternal(w, r, claims, "delete", id, ws) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Search returns the caller's documents nearest to the query text.
func (h *KnowledgeHandler) Search(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "search")
	if !ok {
		return
	}
	var req searchKnowledgeRequest
	if !decodeJSON(w, r, &req) {
		h.recordFailure(r, claims, "search", uuid.Nil, ws, "failed", "malformed_body")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeJSONError(w, http.StatusBadRequest, "query must not be empty")
		h.recordFailure(r, claims, "search", uuid.Nil, ws, "failed", "empty_query")
		return
	}
	if utf8.RuneCountInString(req.Query) > maxContentRunes {
		writeJSONError(w, http.StatusBadRequest, "query is too long")
		h.recordFailure(r, claims, "search", uuid.Nil, ws, "failed", "query_too_long")
		return
	}

	var kind *knowledge.Kind
	if strings.TrimSpace(req.Kind) != "" {
		k, err := knowledge.ValidateKind(strings.TrimSpace(req.Kind))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "kind must be document, campaign_rule or style_guide")
			h.recordFailure(r, claims, "search", uuid.Nil, ws, "failed", "invalid_kind")
			return
		}
		kind = &k
	}
	// An explicit limit outside [1, MaxSearchResults] is rejected rather than
	// silently replaced: substituting a default would hide the caller's mistake
	// and return a result set of a size it did not ask for.
	limit := 0
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 || limit > knowledge.MaxSearchResults {
			writeJSONError(w, http.StatusBadRequest,
				"limit must be between 1 and "+strconv.Itoa(knowledge.MaxSearchResults))
			h.recordFailure(r, claims, "search", uuid.Nil, ws, "failed", "invalid_limit")
			return
		}
	}

	docs, err := h.svc.Search(r.Context(), ws, req.Query, kind, knowledge.NormalizeSearchLimit(limit))
	if err != nil {
		h.fail(w, r, claims, "search", uuid.Nil, ws, err)
		return
	}
	out := make([]knowledgeResponse, 0, len(docs))
	for _, d := range docs {
		out = append(out, newKnowledgeResponse(d))
	}
	if !h.recordOrInternal(w, r, claims, "search", uuid.Nil, ws) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": out, "count": len(out)})
}

// maxContentRunes bounds a search query. It is far below the 64 KiB content
// limit because a query is embedded, and an unbounded query is a cheap way to
// make the embedding step do unbounded work.
const maxContentRunes = 2000

// kind validates the kind on create, where an omitted value means "document".
func (h *KnowledgeHandler) kind(w http.ResponseWriter, r *http.Request, claims *auth.Claims,
	ws uuid.UUID, raw, action string, id uuid.UUID, allowDefault bool) (knowledge.Kind, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if allowDefault {
			return knowledge.KindDocument, true
		}
		writeJSONError(w, http.StatusBadRequest, "kind must not be blank")
		h.recordFailure(r, claims, action, id, ws, "failed", "kind_blank")
		return "", false
	}
	k, err := knowledge.ValidateKind(trimmed)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "kind must be document, campaign_rule or style_guide")
		h.recordFailure(r, claims, action, id, ws, "failed", "invalid_kind")
		return "", false
	}
	return k, true
}

// checkContentLength rejects oversized text before it reaches the domain, so the
// caller gets a 400 naming the field rather than a domain sentinel that has to be
// classified afterwards.
func checkContentLength(w http.ResponseWriter, r *http.Request, claims *auth.Claims, ws uuid.UUID,
	action string, id uuid.UUID, h *KnowledgeHandler, title, content string) error {
	if utf8.RuneCountInString(title) > maxTitleRunes {
		writeJSONError(w, http.StatusBadRequest, "title is too long")
		h.recordFailure(r, claims, action, id, ws, "failed", "title_too_long")
		return errors.New("title too long")
	}
	if len(content) > knowledge.MaxContentLength {
		writeJSONError(w, http.StatusBadRequest, "content exceeds the maximum length")
		h.recordFailure(r, claims, action, id, ws, "failed", "content_too_large")
		return errors.New("content too large")
	}
	return nil
}

// maxTitleRunes bounds a title. Titles are rendered in listings, so an unbounded
// one is a rendering problem as much as a storage one.
const maxTitleRunes = 300

// docID parses the path identifier. A malformed one is a client error: it never
// reaches the store, so it cannot become a 500 or a query.
func (h *KnowledgeHandler) docID(w http.ResponseWriter, r *http.Request, claims *auth.Claims,
	ws uuid.UUID, action string) (uuid.UUID, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	id, err := uuid.Parse(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed knowledge document id")
		h.recordFailure(r, claims, action, uuid.Nil, ws, "failed", "malformed_id")
		return uuid.Nil, false
	}
	return id, true
}

// parseQuery reads the listing parameters.
//
// It parses the raw query string itself rather than calling r.URL.Query(). That
// method swallows its parse error and returns whatever it could read, and since
// Go 1.17 url.ParseQuery rejects ";" as a separator, so a query string containing
// one had the offending pair silently dropped and the request served as though
// the parameter had never been sent. A caller whose cursor was mangled would get
// the first page back and believe it had paged forward. This was a real defect in
// the task listing and is not repeated here.
//
// An unknown parameter is refused rather than ignored, because a caller who
// misspells "cursor" and is silently handed page one learns nothing.
func (h *KnowledgeHandler) parseQuery(w http.ResponseWriter, r *http.Request,
	claims *auth.Claims, ws uuid.UUID) (knowledge.ListQuery, bool) {
	var q knowledge.ListQuery
	known := map[string]bool{"kind": true, "limit": true, "cursor": true}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed query string")
		h.recordFailure(r, claims, "list", uuid.Nil, ws, "failed", "malformed_query")
		return q, false
	}
	for key := range values {
		if !known[key] {
			writeJSONError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
			h.recordFailure(r, claims, "list", uuid.Nil, ws, "failed", "unknown_parameter")
			return q, false
		}
	}
	if raw := values.Get("kind"); raw != "" {
		k, err := knowledge.ValidateKind(strings.TrimSpace(raw))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "kind must be document, campaign_rule or style_guide")
			h.recordFailure(r, claims, "list", uuid.Nil, ws, "failed", "invalid_kind")
			return q, false
		}
		q.Kind = &k
	}
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		// An absent limit means "use the default"; a limit that is present but
		// not a positive integer is a caller mistake and is refused. Quietly
		// turning -1 into the default would hide the error and return a page
		// the caller did not ask for.
		if err != nil || n <= 0 {
			writeJSONError(w, http.StatusBadRequest, "limit must be a positive integer")
			h.recordFailure(r, claims, "list", uuid.Nil, ws, "failed", "invalid_limit")
			return q, false
		}
		q.Limit = n
	}
	if raw := values.Get("cursor"); raw != "" {
		c, err := knowledge.DecodeCursor(raw)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "cursor is not valid")
			h.recordFailure(r, claims, "list", uuid.Nil, ws, "failed", "invalid_cursor")
			return q, false
		}
		q.Before = c
	}
	// Bounds are applied here as well as in the store, so an out-of-range limit
	// is clamped at the boundary and the effective value is what gets reported.
	q.Normalize()
	return q, true
}

// fail classifies a domain error. Client mistakes are 4xx; only something the
// server owns becomes a 500, and its detail is logged rather than returned.
func (h *KnowledgeHandler) fail(w http.ResponseWriter, r *http.Request, claims *auth.Claims,
	action string, id, ws uuid.UUID, err error) {
	switch {
	case errors.Is(err, knowledge.ErrNotFound):
		// 404 rather than 403: the identifier is the document, not the
		// workspace, so answering 403 would confirm to another tenant that the
		// document exists.
		h.recordFailure(r, claims, action, id, ws, "failed", "not_found")
		writeJSONError(w, http.StatusNotFound, "knowledge document not found")
	case errors.Is(err, knowledge.ErrWorkspaceMismatch):
		h.recordFailure(r, claims, action, id, ws, "denied", "workspace_mismatch")
		writeJSONError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, knowledge.ErrInvalidInput),
		errors.Is(err, knowledge.ErrInvalidKind),
		errors.Is(err, knowledge.ErrTooLarge),
		errors.Is(err, knowledge.ErrInvalidCursor):
		h.recordFailure(r, claims, action, id, ws, "failed", "invalid_request")
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		logger.NewEntry("knowledge-handler-error").SetLevel("error").
			With("action", action).WithError(err).Log()
		h.recordFailure(r, claims, action, id, ws, "failed", "internal_error")
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// recordFailure preserves the original client-facing failure while making
// failures to persist the failure audit event visible to operators. The primary
// request has already failed, so there is no safe success response to change;
// dropping the persistence error would only hide an audit-chain gap.
func (h *KnowledgeHandler) recordFailure(r *http.Request, claims *auth.Claims, action string,
	docID, ws uuid.UUID, outcome, detail string) {
	if err := h.record(r, claims, action, docID, ws, outcome, detail); err != nil {
		logger.NewEntry("knowledge-audit-failure-recording-failed").SetLevel("error").
			With("event_type", "knowledge."+action).WithError(err).Log()
	}
}

// record persists a knowledge decision to the audit chain.
func (h *KnowledgeHandler) record(r *http.Request, claims *auth.Claims, action string,
	docID, ws uuid.UUID, outcome, detail string) error {
	if h.auditSink == nil {
		// No sink means no durable record. Reporting success anyway is exactly
		// what the convention forbids, so this is an error and not a no-op.
		return errAuditUnavailable
	}
	rec := audit.Record{
		// The action names are bare verbs -- "create", "search" -- so the event
		// type is built by prefixing rather than by trimming a suffix. Deriving
		// it from a suffix is how the task surface once emitted
		// "task.create-task"; naming it directly removes that class of bug.
		EventType:  "knowledge." + action,
		ActorType:  "user",
		ActorID:    parseActorID(claims.ID),
		TargetType: "knowledge_document",
		TargetID:   docID,
		Outcome:    outcome,
		Principle:  "Privacy by Design",
	}
	if ws != uuid.Nil {
		wsCopy := ws
		rec.WorkspaceID = &wsCopy
	}
	if detail != "" {
		rec.Details = map[string]any{"detail": detail}
	}
	cv := middleware.ExtractContextValues(r)
	if id, err := uuid.Parse(cv.TraceID); err == nil {
		rec.TraceID = id
	}
	if _, err := h.auditSink.Append(r.Context(), rec); err != nil {
		logger.NewEntry("knowledge-audit-persist-failed").SetLevel("error").
			With("event_type", rec.EventType).WithError(err).Log()
		return err
	}
	return nil
}

// recordOrInternal records a successful outcome and fails the request if the
// record could not be written.
//
// This follows the convention AuthHandler.recordAudit established: a
// security-relevant operation must not be able to report success without a
// persisted record. Without this, a create or a delete whose audit entry failed
// would answer 201 or 204 and the trail would simply have a hole in it -- which
// is worse than an error, because nothing downstream can tell the hole exists.
//
// It applies to successes only. A denial still returns its denial when the
// record fails: the authorization control did its job, and turning a 403 into a
// 500 would obscure the outcome the caller actually needs. The failure is logged
// either way.
func (h *KnowledgeHandler) recordOrInternal(w http.ResponseWriter, r *http.Request,
	claims *auth.Claims, action string, docID, ws uuid.UUID) bool {
	if err := h.record(r, claims, action, docID, ws, "success", ""); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "audit unavailable")
		return false
	}
	return true
}
