// Package webui serves the AUSTRO OS browser application (ADR-016).
//
// The application is a static document, one script and one stylesheet, compiled
// into the binary. It carries no data: every value a user sees is fetched from
// the authenticated JSON API, and the server alone decides what a caller may
// see. There is deliberately no second backend, no build step, and no runtime
// dependency beyond the existing API.
//
// These routes are the only public routes besides the health probes and the
// unauthenticated auth entry points. That is safe because they return fixed
// assets and nothing else -- no handler here reads a request body, a query
// parameter, a path value, or a cookie.
package webui

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed all:static
var staticFS embed.FS

// Asset is one static file the application serves.
type Asset struct {
	// Method is always GET; HEAD is handled by net/http automatically.
	Method string
	// Pattern is the URL path.
	Pattern string
	// File is the path inside the embedded filesystem.
	File string
	// ContentType is sent verbatim, so the browser never has to sniff.
	ContentType string
}

// Assets lists every static route the browser application serves. It is the
// single source of truth for the UI surface, mirroring internal/api.Routes for
// the JSON API: main.go registers exactly these, and they are exactly the paths
// the authorization layer exempts as public.
func Assets() []Asset {
	return []Asset{
		{http.MethodGet, "/", "static/index.html", "text/html; charset=utf-8"},
		{http.MethodGet, "/assets/app.js", "static/app.js", "text/javascript; charset=utf-8"},
		{http.MethodGet, "/assets/styles.css", "static/styles.css", "text/css; charset=utf-8"},
	}
}

// contentSecurityPolicy is deliberately strict. The application uses no inline
// script and no inline style, so both can be locked to 'self'; 'unsafe-inline'
// and 'unsafe-eval' never appear. connect-src 'self' confines fetch to this
// origin, which is also what keeps the UI from being turned into a data
// exfiltration channel if a script were ever injected. frame-ancestors 'none'
// blocks clickjacking without relying on X-Frame-Options alone.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Load reads every asset at startup. A missing or unreadable embedded file is a
// build defect, and failing here turns it into a startup error instead of a
// runtime 404 on a page the user cannot diagnose.
func Load() (map[Asset][]byte, error) {
	loaded := make(map[Asset][]byte)
	for _, a := range Assets() {
		b, err := fs.ReadFile(staticFS, a.File)
		if err != nil {
			return nil, fmt.Errorf("webui: read %s: %w", a.File, err)
		}
		if len(b) == 0 {
			return nil, fmt.Errorf("webui: %s is empty", a.File)
		}
		loaded[a] = b
	}
	return loaded, nil
}

// Handler returns a handler that writes one preloaded asset. The response is
// fixed content with security headers; nothing from the request influences it.
func Handler(a Asset, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", a.ContentType)
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// The bundle is not content-hashed, so a cached copy could outlive a
		// deployment and serve stale script against a changed API. Revalidation
		// is cheap at this size and removes that class of failure.
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// ContentSecurityPolicy exposes the policy so tests can assert the served
// header matches the one this package intends to send.
func ContentSecurityPolicy() string { return contentSecurityPolicy }
