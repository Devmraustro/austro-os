package api

import (
	"net/http"

	"austro-os/internal/auth"
)

// RequireAuth enriches the request with verified claims from an
// Authorization: Bearer <token> header and fails closed on a malformed or
// invalid presented token (401). A request with no header passes through
// unauthenticated: the deny-by-default authorization layer then denies any
// route without an explicit public exemption, and public endpoints (health,
// login, refresh, logout, bootstrap) keep working without credentials. An
// invalid token is never silently ignored.
func (h *AuthHandler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if header == "" {
			next.ServeHTTP(w, r)
			return
		}
		claims, ok := auth.BearerClaims(header, h.jwt.VerifyAccessToken)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, auth.WithClaims(r, claims))
	})
}
