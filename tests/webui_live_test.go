package austro_os_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBrowserApplicationIsServed verifies the ADR-016 browser application is
// actually reachable from a browser: the document, the script and the stylesheet
// are served unauthenticated, with the security headers the page's
// Content-Security-Policy depends on.
//
// These routes are public by necessity -- a login page cannot require a token --
// so the paired assertions below matter as much as these: adding public routes
// must not have opened any route that carries data.
func TestBrowserApplicationIsServed(t *testing.T) {
	c := &http.Client{Timeout: 10 * time.Second}
	base := apiURL()

	t.Run("document", func(t *testing.T) {
		resp, err := c.Get(base + "/")
		require.NoError(t, err, "the browser application must be reachable at /")
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))

		csp := resp.Header.Get("Content-Security-Policy")
		require.NotEmpty(t, csp, "the document must be served with a Content-Security-Policy")
		require.Contains(t, csp, "default-src 'none'")
		require.NotContains(t, csp, "unsafe-inline",
			"unsafe-inline would let an injected script run; the application uses no inline code")
		require.NotContains(t, csp, "unsafe-eval")
		require.Contains(t, csp, "frame-ancestors 'none'", "clickjacking protection")
		require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		html := string(body)
		require.Contains(t, html, `id="auth-view"`, "the login view must be present")
		require.Contains(t, html, `src="/assets/app.js"`, "the document must load its script")
		// The document carries no data: it is a shell the script fills from the
		// authenticated API.
		require.NotContains(t, html, "access_token", "the document must not embed token material")
	})

	t.Run("script and stylesheet", func(t *testing.T) {
		for path, contentType := range map[string]string{
			"/assets/app.js":     "text/javascript; charset=utf-8",
			"/assets/styles.css": "text/css; charset=utf-8",
		} {
			resp, err := c.Get(base + path)
			require.NoError(t, err, "%s must be reachable", path)
			require.Equal(t, http.StatusOK, resp.StatusCode, "%s", path)
			require.Equal(t, contentType, resp.Header.Get("Content-Type"), "%s", path)
			require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"), "%s", path)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})

	// A method an asset does not serve must not be answered. Note this is 403
	// and not the 405 the mux would return: authorization runs before routing,
	// so an unauthenticated POST is refused by the authorization layer and never
	// reaches the router. main_test.go pins that ordering locally.
	t.Run("assets are GET only", func(t *testing.T) {
		resp, err := c.Post(base+"/assets/app.js", "application/json", strings.NewReader("{}"))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode,
			"a POST to a static asset must be refused, not served")
	})
}

// TestPublicSurfaceDoesNotLeakData is the security half of the UI change. The
// browser application required new public routes; this asserts that the
// authorization layer still refuses every data route without credentials, and
// that path tricks do not reach a protected handler through a public prefix.
func TestPublicSurfaceDoesNotLeakData(t *testing.T) {
	c := &http.Client{Timeout: 10 * time.Second}
	base := apiURL()

	for _, path := range []string{"/api/me", "/workspaces", "/workspaces/00000000-0000-4000-8000-000000000000"} {
		resp, err := c.Get(base + path)
		require.NoError(t, err, "%s", path)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode,
			"%s must be refused without credentials (deny by default)", path)
	}

	// An unregistered path is refused by the authorizer before it can reach the
	// mux, so it is 403 rather than 404: no rule matches, and deny-by-default
	// answers first. That ordering is deliberate -- it means the route table
	// cannot be probed for existence.
	resp, err := c.Get(base + "/api/does-not-exist")
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"an unknown route must be denied by the authorization layer, not routed")

	// The static prefix must not become a way to reach the API.
	for _, probe := range []string{"/assets/", "/assets/../../api/me", "/./api/me"} {
		resp, err := c.Get(base + probe)
		if err != nil {
			continue // the client refuses some malformed URLs before sending
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NotEqual(t, http.StatusOK, resp.StatusCode,
			"probe %q must not be served successfully", probe)
		require.NotContains(t, string(body), "access_token",
			"probe %q must not return authenticated data", probe)
	}
}
