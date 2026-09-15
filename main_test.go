package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"austro-os/internal/api"
	"austro-os/internal/authz"
	"austro-os/internal/rbac"
	"austro-os/internal/webui"
)

// stubHandlers returns a handler for every declared route. The handlers are
// trivial on purpose: these tests are about the wiring -- which paths are
// registered, which are exempt from authorization, and what the router does
// when the two lists disagree -- not about handler behaviour, which the
// internal/api tests and the live integration suite cover.
func stubHandlers() map[api.Route]http.HandlerFunc {
	h := make(map[api.Route]http.HandlerFunc)
	for _, r := range api.Routes() {
		route := r
		h[route] = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Test-Route", route.String())
			w.WriteHeader(http.StatusOK)
		}
	}
	return h
}

func mustAssets(t *testing.T) map[webui.Asset][]byte {
	t.Helper()
	assets, err := webui.Load()
	if err != nil {
		t.Fatalf("webui.Load: %v", err)
	}
	return assets
}

func TestRegisterRoutesServesEveryDeclaredRoute(t *testing.T) {
	mux := http.NewServeMux()
	if err := registerRoutes(mux, stubHandlers(), mustAssets(t)); err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	for _, route := range api.Routes() {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(route.Method, route.Pattern, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 (route declared but not served)", route, rec.Code)
		}
		if got := rec.Header().Get("X-Test-Route"); got != route.String() {
			t.Errorf("%s: reached handler for %q", route, got)
		}
	}
}

func TestRegisterRoutesServesTheBrowserAssets(t *testing.T) {
	mux := http.NewServeMux()
	if err := registerRoutes(mux, stubHandlers(), mustAssets(t)); err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	for _, asset := range webui.Assets() {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, asset.Pattern, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", asset.Pattern, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != asset.ContentType {
			t.Errorf("%s: Content-Type %q, want %q", asset.Pattern, got, asset.ContentType)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != webui.ContentSecurityPolicy() {
			t.Errorf("%s: missing or wrong Content-Security-Policy: %q", asset.Pattern, got)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: served an empty body", asset.Pattern)
		}
	}
}

// TestRegisterRoutesRejectsADeclaredRouteWithNoHandler is the guard that keeps
// api/openapi.yaml honest. The parity test compares the spec against
// api.Routes(); if a declared route could silently lack a handler, the spec
// could stay "in parity" while documenting an endpoint that 404s -- exactly the
// eight phantom operations this replaced.
func TestRegisterRoutesRejectsADeclaredRouteWithNoHandler(t *testing.T) {
	handlers := stubHandlers()
	var missing api.Route
	for r := range handlers {
		missing = r
		break
	}
	delete(handlers, missing)

	err := registerRoutes(http.NewServeMux(), handlers, mustAssets(t))
	if err == nil {
		t.Fatalf("expected an error for the declared route with no handler (%s)", missing)
	}
	if !strings.Contains(err.Error(), missing.String()) {
		t.Errorf("error should name the offending route %s, got: %v", missing, err)
	}
}

func TestRegisterRoutesRejectsAHandlerWithNoRoute(t *testing.T) {
	handlers := stubHandlers()
	handlers[api.Route{Method: http.MethodGet, Pattern: "/not-declared"}] = func(http.ResponseWriter, *http.Request) {}

	err := registerRoutes(http.NewServeMux(), handlers, mustAssets(t))
	if err == nil {
		t.Fatal("expected an error for a handler whose route is not declared")
	}
	if !strings.Contains(err.Error(), "/not-declared") {
		t.Errorf("error should name the offending route, got: %v", err)
	}
}

func TestRegisterRoutesRejectsAnUnloadedAsset(t *testing.T) {
	err := registerRoutes(http.NewServeMux(), stubHandlers(), map[webui.Asset][]byte{})
	if err == nil {
		t.Fatal("expected an error when a declared asset was never loaded")
	}
	if !strings.Contains(err.Error(), "was not loaded") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestPublicSurfaceIsExactlyTheIntendedSet pins the single most dangerous
// function in this file. isPublicEndpoint decides what an unauthenticated
// caller may reach, and it is not reachable from any other package, so without
// this it would be untested. Every declared route and every asset is checked,
// so a new exemption cannot appear without a test change.
func TestPublicSurfaceIsExactlyTheIntendedSet(t *testing.T) {
	publicByDesign := map[string]bool{
		"GET /health/live":         true,
		"GET /health/ready":        true,
		"POST /api/auth/login":     true,
		"POST /api/auth/refresh":   true,
		"POST /api/auth/logout":    true,
		"POST /api/auth/bootstrap": true,
	}

	for _, route := range api.Routes() {
		got := isPublicEndpoint(route.Method, route.Pattern)
		want := publicByDesign[route.String()]
		if got != want {
			t.Errorf("isPublicEndpoint(%s, %s) = %v, want %v", route.Method, route.Pattern, got, want)
		}
	}

	// HEAD is served by net/http for every GET pattern, so it must carry the
	// same exemption -- and no more.
	for _, route := range api.Routes() {
		if route.Method != http.MethodGet {
			continue
		}
		got := isPublicEndpoint(http.MethodHead, route.Pattern)
		want := publicByDesign["GET "+route.Pattern]
		if got != want {
			t.Errorf("isPublicEndpoint(HEAD, %s) = %v, want %v (must mirror GET)", route.Pattern, got, want)
		}
	}

	// The static assets are public for GET/HEAD only. A public POST here would
	// be a wildcard exemption rather than a route exemption.
	for _, asset := range webui.Assets() {
		if !isPublicEndpoint(http.MethodGet, asset.Pattern) {
			t.Errorf("asset %s must be public for GET", asset.Pattern)
		}
		if !isPublicEndpoint(http.MethodHead, asset.Pattern) {
			t.Errorf("asset %s must be public for HEAD", asset.Pattern)
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if isPublicEndpoint(m, asset.Pattern) {
				t.Errorf("asset %s must not be public for %s", asset.Pattern, m)
			}
		}
	}
}

// TestRouterDeniesByDefault exercises the real middleware chain over the real
// router: an unauthenticated request reaches the authorization layer, which
// refuses anything without an explicit rule. Note the ordering this proves --
// authorization runs before routing, so a method an asset does not serve is
// denied (403) rather than answered 405 by the mux.
func TestRouterDeniesByDefault(t *testing.T) {
	mux := http.NewServeMux()
	if err := registerRoutes(mux, stubHandlers(), mustAssets(t)); err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	service := authz.NewAuthorizer()
	service.AddRules(rbac.ImplementedRules())
	chain := authzMiddleware(service, mux, nil)

	allowed := []struct{ method, path string }{
		{http.MethodGet, "/health/live"},
		{http.MethodGet, "/health/ready"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/assets/app.js"},
		{http.MethodGet, "/assets/styles.css"},
	}
	for _, a := range allowed {
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, httptest.NewRequest(a.method, a.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status %d, want 200", a.method, a.path, rec.Code)
		}
	}

	denied := []struct{ method, path string }{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/workspaces"},
		{http.MethodPost, "/workspaces"},
		{http.MethodGet, "/workspaces/00000000-0000-4000-8000-000000000000"},
		{http.MethodPost, "/assets/app.js"},
		{http.MethodGet, "/api/does-not-exist"},
		{http.MethodGet, "/assets/"},
	}
	for _, d := range denied {
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, httptest.NewRequest(d.method, d.path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403 (deny by default)", d.method, d.path, rec.Code)
		}
	}
}
