package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"austro-os/internal/auth"
	"austro-os/internal/middleware"
	"austro-os/internal/publish"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
)

// PublicationHandler exposes the smallest browser-facing Publishing contract:
// create, bounded list, get, submit for review, approve, reject and publish.
// There is deliberately no generic status PATCH, delete, search or
// organization-wide operation: lifecycle authority remains in the domain.
type PublicationHandler struct {
	svc *publish.Service
}

func NewPublicationHandler(svc *publish.Service) *PublicationHandler {
	return &PublicationHandler{svc: svc}
}

type createPublicationRequest struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Platform string `json:"platform"`
}

type publicationListResponse struct {
	Publications []*publish.Publication `json:"publications"`
	Count        int                    `json:"count"`
	Limit        int                    `json:"limit"`
}

const (
	publicationTitleMaxRunes = 200
	publicationBodyMaxRunes  = 10000
	publicationPlatformMax   = 64
	publicationPageSize      = 100
)

// Create queues one publication. The Idempotency-Key header is scoped to the
// caller's workspace and makes browser retries/double submits return the same
// aggregate rather than creating a second delivery record.
func (h *PublicationHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "create-publication")
	if !ok {
		return
	}
	var req createPublicationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validPublicationText(w, req.Title, req.Body, req.Platform) {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 128 {
		writeJSONError(w, http.StatusBadRequest, "idempotency key is too long")
		return
	}
	ctx := publicationContext(r, claims)
	p, err := h.svc.CreateWithIdempotency(ctx, ws, nil, nil, req.Title, req.Body, req.Platform, key)
	if err != nil {
		h.publicationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// List returns a deterministic, bounded newest-first page. The database store
// enforces the hard ceiling even if a future caller forgets to apply this one.
func (h *PublicationHandler) List(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "list-publications")
	if !ok {
		return
	}
	status, limit, ok := parsePublicationQuery(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(publicationContext(r, claims), ws, status)
	if err != nil {
		h.publicationError(w, err)
		return
	}
	if len(list) > limit {
		list = list[:limit]
	}
	writeJSON(w, http.StatusOK, publicationListResponse{Publications: list, Count: len(list), Limit: limit})
}

// Get returns one publication in the caller's workspace.
func (h *PublicationHandler) Get(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "get-publication")
	if !ok {
		return
	}
	id, ok := publicationID(w, r)
	if !ok {
		return
	}
	p, err := h.svc.Get(publicationContext(r, claims), ws, id)
	if err != nil {
		h.publicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// Submit moves queued content to review. It accepts no status field or body.
func (h *PublicationHandler) Submit(w http.ResponseWriter, r *http.Request) {
	claims, ws, ok := h.scope(w, r, "submit-publication")
	if !ok {
		return
	}
	if !emptyBody(w, r) {
		return
	}
	id, ok := publicationID(w, r)
	if !ok {
		return
	}
	p, err := h.svc.ToReview(publicationContext(r, claims), ws, id)
	if err != nil {
		h.publicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *PublicationHandler) Approve(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, "approve-publication", func(ctx context.Context, ws, id uuid.UUID, actor string) (*publish.Publication, error) {
		return h.svc.Approve(ctx, ws, id, actor)
	})
}

func (h *PublicationHandler) Reject(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, "reject-publication", func(ctx context.Context, ws, id uuid.UUID, actor string) (*publish.Publication, error) {
		return h.svc.Reject(ctx, ws, id, actor)
	})
}

func (h *PublicationHandler) Publish(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, "publish-publication", func(ctx context.Context, ws, id uuid.UUID, actor string) (*publish.Publication, error) {
		return h.svc.Publish(ctx, ws, id, actor)
	})
}

func (h *PublicationHandler) Retry(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, "retry-publication", func(ctx context.Context, ws, id uuid.UUID, actor string) (*publish.Publication, error) {
		return h.svc.Retry(ctx, ws, id, actor)
	})
}

type publicationDecision func(context.Context, uuid.UUID, uuid.UUID, string) (*publish.Publication, error)

func (h *PublicationHandler) decide(w http.ResponseWriter, r *http.Request, action string, fn publicationDecision) {
	claims, ws, ok := h.scope(w, r, action)
	if !ok {
		return
	}
	if claims.Role != rbac.RoleWorkspaceAdmin {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !emptyBody(w, r) {
		return
	}
	id, ok := publicationID(w, r)
	if !ok {
		return
	}
	p, err := fn(publicationContext(r, claims), ws, id, claims.ID)
	if err != nil {
		h.publicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *PublicationHandler) scope(w http.ResponseWriter, r *http.Request, _ string) (*auth.Claims, uuid.UUID, bool) {
	claims, ok := auth.FromRequest(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return nil, uuid.Nil, false
	}
	// The middleware authorizes the route, but handlers also validate the
	// identity boundary so direct/unit use cannot turn malformed claims into a
	// workspace query or an audit actor.
	if !rbac.ValidRole(claims.Role) || claims.ID == "" {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	if _, err := uuid.Parse(claims.ID); err != nil {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	workspace, err := WorkspaceFromClaims(claims)
	if err != nil {
		// Founders intentionally have no workspace-scoped Publishing authority.
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	ws, err := uuid.Parse(workspace)
	if err != nil || ws == uuid.Nil {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return nil, uuid.Nil, false
	}
	return claims, ws, true
}

func publicationContext(r *http.Request, claims *auth.Claims) context.Context {
	cv := middleware.ExtractContextValues(r)
	return publish.WithActor(publish.WithTrace(r.Context(), cv.TraceID, cv.SpanID), claims.ID)
}

func publicationID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || id == uuid.Nil {
		writeJSONError(w, http.StatusBadRequest, "invalid publication id")
		return uuid.Nil, false
	}
	return id, true
}

func validPublicationText(w http.ResponseWriter, title, body, platform string) bool {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" {
		writeJSONError(w, http.StatusBadRequest, "title and body are required")
		return false
	}
	if utf8.RuneCountInString(title) > publicationTitleMaxRunes || utf8.RuneCountInString(body) > publicationBodyMaxRunes {
		writeJSONError(w, http.StatusBadRequest, "publication content is too long")
		return false
	}
	if strings.TrimSpace(platform) == "" {
		writeJSONError(w, http.StatusBadRequest, "platform is required")
		return false
	}
	if utf8.RuneCountInString(platform) > publicationPlatformMax {
		writeJSONError(w, http.StatusBadRequest, "platform is too long")
		return false
	}
	return true
}

func parsePublicationQuery(w http.ResponseWriter, r *http.Request) (*publish.Status, int, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed query string")
		return nil, 0, false
	}
	for key := range values {
		if key != "status" && key != "limit" {
			writeJSONError(w, http.StatusBadRequest, "unsupported query parameter: "+key)
			return nil, 0, false
		}
	}
	var status *publish.Status
	if raw := strings.TrimSpace(values.Get("status")); raw != "" {
		st := publish.Status(raw)
		if !publish.ValidStatus(st) {
			writeJSONError(w, http.StatusBadRequest, "invalid publication status")
			return nil, 0, false
		}
		status = &st
	}
	limit := 50
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > publicationPageSize {
			writeJSONError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return nil, 0, false
		}
		limit = n
	}
	return status, limit, true
}

func emptyBody(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil || len(body) != 0 {
		writeJSONError(w, http.StatusBadRequest, "operation does not accept a request body")
		return false
	}
	return true
}

func (h *PublicationHandler) publicationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, publish.ErrNotFound), errors.Is(err, publish.ErrWorkspaceMismatch):
		writeJSONError(w, http.StatusNotFound, "publication not found")
	case errors.Is(err, publish.ErrRateLimited):
		writeJSONError(w, http.StatusTooManyRequests, "publishing rate limit exceeded")
	case errors.Is(err, publish.ErrInvalidInput), errors.Is(err, publish.ErrEmptyContent),
		errors.Is(err, publish.ErrInvalidStatus), errors.Is(err, publish.ErrInvalidTransition),
		errors.Is(err, publish.ErrTerminalState), errors.Is(err, publish.ErrApprovalRequired),
		errors.Is(err, publish.ErrUnauthorizedApprover), errors.Is(err, publish.ErrUnauthorizedRejecter),
		errors.Is(err, publish.ErrUnauthorizedPublisher):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		// Adapter failures are intentionally not reflected in the response. The
		// durable publication record contains the bounded failure reason and the
		// GET/status journey is the safe inspection path.
		writeJSONError(w, http.StatusBadGateway, "publication delivery failed")
	}
}
