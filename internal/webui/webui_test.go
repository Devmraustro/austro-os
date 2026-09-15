package webui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestLoadReturnsEveryAsset(t *testing.T) {
	assets, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, a := range Assets() {
		b, ok := assets[a]
		if !ok {
			t.Errorf("asset %s %s was not loaded", a.Method, a.Pattern)
			continue
		}
		if len(b) == 0 {
			t.Errorf("asset %s is empty", a.File)
		}
	}
	if len(assets) != len(Assets()) {
		t.Errorf("loaded %d assets, declared %d", len(assets), len(Assets()))
	}
}

func TestHandlerSetsSecurityHeaders(t *testing.T) {
	assets, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, a := range Assets() {
		rec := httptest.NewRecorder()
		Handler(a, assets[a])(rec, httptest.NewRequest(http.MethodGet, a.Pattern, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", a.Pattern, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != a.ContentType {
			t.Errorf("%s: Content-Type = %q, want %q", a.Pattern, got, a.ContentType)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy() {
			t.Errorf("%s: Content-Security-Policy = %q", a.Pattern, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", a.Pattern, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", a.Pattern, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy = %q, want no-referrer", a.Pattern, got)
		}
	}
}

// TestPolicyForbidsInlineExecution pins the two properties that make the policy
// worth having. If 'unsafe-inline' or 'unsafe-eval' ever appears, the policy
// stops containing an injected script, and the rest of the header is decoration.
func TestPolicyForbidsInlineExecution(t *testing.T) {
	p := ContentSecurityPolicy()
	for _, banned := range []string{"unsafe-inline", "unsafe-eval", "*", "data:"} {
		if strings.Contains(p, banned) {
			t.Errorf("Content-Security-Policy must not contain %q: %s", banned, p)
		}
	}
	for _, required := range []string{"default-src 'none'", "frame-ancestors 'none'", "connect-src 'self'"} {
		if !strings.Contains(p, required) {
			t.Errorf("Content-Security-Policy is missing %q: %s", required, p)
		}
	}
}

// TestHTMLHasNoInlineScriptOrStyle keeps the document compatible with the policy
// it is served under. An inline handler or <style> block would not merely be a
// style problem: under this CSP the browser refuses to run it, so the page would
// break in production in a way no server-side test would notice.
func TestHTMLHasNoInlineScriptOrStyle(t *testing.T) {
	assets, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var html Asset
	for _, a := range Assets() {
		if strings.HasSuffix(a.File, "index.html") {
			html = a
		}
	}
	if html.File == "" {
		t.Fatal("no index.html asset is declared")
	}
	body := stripHTMLComments(string(assets[html]))

	if re := regexp.MustCompile(`(?i)<script[^>]*>`); true {
		for _, m := range re.FindAllString(body, -1) {
			if !strings.Contains(m, `src="/assets/`) {
				t.Errorf("inline or non-local <script> is not allowed under this CSP: %s", m)
			}
		}
	}
	if re := regexp.MustCompile(`(?i)<style[^>]*>`); re.MatchString(body) {
		t.Error("inline <style> is not allowed under this CSP; use /assets/styles.css")
	}
	// Inline event handlers are blocked by script-src 'self' and are the easiest
	// way to reintroduce an injection sink.
	if re := regexp.MustCompile(`(?i)\son[a-z]+\s*=`); re.MatchString(body) {
		t.Error("inline event handler attributes (onclick=, onsubmit=, ...) are not allowed under this CSP")
	}
	// A remote origin in the document would be blocked by the policy anyway, and
	// its presence means the UI depends on something outside this deployment.
	if re := regexp.MustCompile(`(?i)(src|href)\s*=\s*"https?://`); re.MatchString(body) {
		t.Error("the document must not reference an external origin")
	}
}

// TestScriptNeverLogsCredentials guards the redaction rule on the client side:
// the token and the password must not reach the console or the URL.
func TestScriptNeverLogsCredentials(t *testing.T) {
	assets, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var js Asset
	for _, a := range Assets() {
		if strings.HasSuffix(a.File, "app.js") {
			js = a
		}
	}
	if js.File == "" {
		t.Fatal("no app.js asset is declared")
	}
	src := stripJSComments(string(assets[js]))
	if re := regexp.MustCompile(`console\.(log|info|debug|warn|error)\(`); re.MatchString(src) {
		t.Error("the client must not write to the console: request and token material could leak into logs")
	}
	for _, banned := range []string{"localStorage", "document.cookie", "location.hash", "location.search"} {
		if strings.Contains(src, banned) {
			t.Errorf("the client must not use %s for session material", banned)
		}
	}
	if !strings.Contains(src, "sessionStorage") {
		t.Error("session material is expected to live in sessionStorage, scoped to the tab")
	}
}

// TestOpeningThePageIsNotASideEffect pins a defect this test was written after.
// The login view used to issue POST /api/auth/bootstrap as a "status probe" on
// load. That endpoint is unauthenticated and state-changing: on a fresh install
// it creates the Founder, on an initialized one it writes an
// "already_initialized" audit row, and either way it spends a rate-limit token.
// Opening the login page therefore mutated the system. A mutating call is never
// a valid probe, and nothing on the load path may send one.
func TestOpeningThePageIsNotASideEffect(t *testing.T) {
	assets, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var js Asset
	for _, a := range Assets() {
		if strings.HasSuffix(a.File, "app.js") {
			js = a
		}
	}
	src := stripJSComments(string(assets[js]))

	// The load path for an unauthenticated visitor is initAuthView(); it must
	// not reach the network at all.
	body, ok := functionBody(src, "initAuthView")
	if !ok {
		t.Fatal("initAuthView is missing: the unauthenticated load path is unknown")
	}
	for _, call := range []string{"request(", "fetch(", "authenticated("} {
		if strings.Contains(body, call) {
			t.Errorf("the login view issues %s on load; opening the page must not send a request", call)
		}
	}

	// Bootstrap must be reachable from exactly one place, the explicit button.
	// A second occurrence means something else is calling a state-changing,
	// unauthenticated endpoint on the user's behalf.
	if n := strings.Count(src, `"/api/auth/bootstrap"`); n != 1 {
		t.Errorf("POST /api/auth/bootstrap is referenced %d times, want exactly 1 (the explicit button)", n)
	}

	// Every mutating call the client can make must be an action the operator
	// asked for. Anything else in this list is a request the UI sends because
	// it felt like it, which is how the defect above happened.
	allowed := map[string]bool{
		`"/api/auth/login"`:     true, // login form submit
		`"/api/auth/logout"`:    true, // sign-out button
		`"/api/auth/refresh"`:   true, // only after a 401 on a call the user made
		`"/api/auth/bootstrap"`: true, // explicit bootstrap button
		`"/workspaces"`:         true, // create-workspace form submit
		`"/tasks"`:              true, // create-task form submit
		// The transition path is built by concatenation because the task id is
		// dynamic: "/tasks/" + encodeURIComponent(id) + "/transition". The
		// regex below captures the first quoted literal, so this call site
		// registers as "/tasks/". It is the click handler on a per-row
		// transition button, and it is confirmed with the operator first when
		// the destination is a terminal stage.
		`"/tasks/"`: true,
		// Knowledge, same reasoning. "/knowledge" is the create form,
		// "/knowledge/search" the search box, and "/knowledge/" the literal
		// prefix of the two concatenated per-document paths -- the edit form
		// submit (PATCH) and the Delete button, which is confirmed first
		// because knowledge has no archive to move a document into instead.
		`"/knowledge"`:        true,
		`"/knowledge/search"`: true,
		`"/knowledge/"`:       true,
		// Publishing: draft creation is the create form; the dynamic prefix
		// covers submit/approve/reject/publish/retry buttons.
		`"/publications"`:  true,
		`"/publications/"`: true,
		// Creator: start is the explicit form; approve/retry are row actions.
		`"/pipelines"`:  true,
		`"/pipelines/"`: true,
	}
	re := regexp.MustCompile(`(?:request|authenticated)\(\s*"(POST|PUT|PATCH|DELETE)"\s*,\s*("[^"]*")`)
	seen := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		seen[m[2]]++
		if !allowed[m[2]] {
			t.Errorf("unexpected mutating call %s %s: only operator-initiated actions may mutate", m[1], m[2])
		}
	}
	if len(seen) == 0 {
		t.Error("no mutating call sites were parsed; the assertion above is vacuous")
	}
}

// functionBody returns the body of `function <name>(...) { ... }`, matching
// braces and skipping string literals so a brace in a string cannot end it.
func functionBody(src, name string) (string, bool) {
	marker := "function " + name + "("
	start := strings.Index(src, marker)
	if start < 0 {
		return "", false
	}
	open := strings.Index(src[start:], "{")
	if open < 0 {
		return "", false
	}
	i, depth := start+open, 0
	for ; i < len(src); i++ {
		switch c := src[i]; c {
		case '"', '\'', '`':
			i++
			for i < len(src) && src[i] != c {
				if src[i] == '\\' {
					i++
				}
				i++
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start+open+1 : i], true
			}
		}
	}
	return "", false
}

// stripJSComments removes // and /* */ comments while leaving string literals
// intact. Without it, a comment explaining that the code deliberately avoids
// localStorage is indistinguishable from the code using it.
func stripJSComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '"' || c == '\'' || c == '`':
			b.WriteByte(c)
			i++
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					b.WriteByte(s[i])
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				b.WriteByte(s[i])
				if s[i] == c {
					i++
					break
				}
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 < len(s) {
				i += 2
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// stripHTMLComments removes <!-- ... --> so documentation inside the markup does
// not trip the structural assertions above.
func stripHTMLComments(s string) string {
	re := regexp.MustCompile(`(?s)<!--.*?-->`)
	return re.ReplaceAllString(s, "")
}
