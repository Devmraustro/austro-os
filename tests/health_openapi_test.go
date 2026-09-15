package austro_os_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"austro-os/internal/principlemapping"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// openAPISummary holds the structural facts we assert about the spec.
type openAPISummary struct {
	openapi  string
	title    string
	totalOps int
	ops      []openAPIOpSummary // path, method, operationId, principles, security
}

type openAPIOpSummary struct {
	path        string
	method      string
	operationID string
	principles  []string
	hasAuth     bool
}

// loadOpenAPISummary parses the spec with yaml.Node so that path items appearing
// multiple times (one per HTTP method) are merged by walking the node tree.
func loadOpenAPISummary(t *testing.T) *openAPISummary {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "api", "openapi.yaml"))
	require.NoError(t, err)

	var root yaml.Node
	require.NoError(t, yaml.Unmarshal(b, &root))
	require.Equal(t, yaml.DocumentNode, root.Kind)

	doc := root.Content[0]
	require.Equal(t, yaml.MappingNode, doc.Kind)

	summary := &openAPISummary{}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key := doc.Content[i].Value
		val := doc.Content[i+1]

		switch key {
		case "openapi":
			summary.openapi = val.Value
		case "info":
			for j := 0; j+1 < len(val.Content); j += 2 {
				if val.Content[j].Value == "title" {
					summary.title = val.Content[j+1].Value
				}
			}
		case "paths":
			walkPaths(t, summary, val.Content)
		}
	}
	return summary
}

// walkPaths handles the paths node, whose children are path keys -> path items
// that may repeat across the spec; each path item maps method -> operation.
func walkPaths(t *testing.T, summary *openAPISummary, pathItems []*yaml.Node) {
	t.Helper()
	for i := 0; i+1 < len(pathItems); i += 2 {
		path := pathItems[i].Value
		item := pathItems[i+1]
		if item.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			method := item.Content[j].Value
			op := item.Content[j+1]
			if !isHTTPMethod(method) {
				continue
			}
			s := openAPIOpSummary{path: path, method: method}
			for k := 0; k+1 < len(op.Content); k += 2 {
				fk := op.Content[k].Value
				fv := op.Content[k+1]
				switch fk {
				case "operationId":
					s.operationID = fv.Value
				case "principles":
					for _, p := range fv.Content {
						if p.Kind == yaml.ScalarNode {
							s.principles = append(s.principles, p.Value)
						}
					}
				case "security":
					if len(fv.Content) > 0 && fv.Content[0].Kind == yaml.MappingNode {
						for a := 0; a+1 < len(fv.Content[0].Content); a += 2 {
							if fv.Content[0].Content[a].Value == "bearerAuth" {
								s.hasAuth = true
							}
						}
					}
				}
			}
			summary.totalOps++
			summary.ops = append(summary.ops, s)
		}
	}
}

func isHTTPMethod(m string) bool {
	switch m {
	case "get", "post", "put", "patch", "delete", "head", "options":
		return true
	}
	return false
}

// TestOpenAPIPrincipleReferences verifies the OpenAPI contract gate: every
// operation carries a non-empty "principles:" list (100% coverage), and
// protected operations require bearerAuth.
func TestOpenAPIPrincipleReferences(t *testing.T) {
	spec := loadOpenAPISummary(t)

	require.Equal(t, "3.1.0", spec.openapi, "spec must use OpenAPI 3.1.0")
	require.Equal(t, "AUSTRO OS API", spec.title)

	// A floor, not an exact count. The exact surface is pinned structurally by
	// TestOpenAPIMatchesRegisteredRoutes, which compares this document against
	// api.Routes() in both directions; repeating the number here would only
	// mean editing two places every time a capability is added, and the parity
	// test is the one that can actually catch a phantom or a missing entry.
	require.GreaterOrEqual(t, spec.totalOps, 13, "expected a richly-specified API surface")

	for _, op := range spec.ops {
		// Public, read-only surface: health probes and the constitutional
		// principles registry (the constitution is publicly inspectable by
		// design). Business operations must declare + enforce a principle and
		// require bearerAuth.
		if isPublicReadOnlyPath(op.path) {
			continue
		}
		require.NotEmpty(t, op.principles,
			"operation %s %s must declare principles", op.method, op.path)
		for _, p := range op.principles {
			require.NotContains(t, p, "TODO", "principle must not be a placeholder")
		}
		require.True(t, op.hasAuth,
			"operation %s %s must require bearerAuth", op.method, op.path)
	}
}

// isPublicReadOnlyPath reports whether an endpoint is public and unauthenticated
// (infrastructure probes, the constitutional registry, and auth token endpoints).
func isPublicReadOnlyPath(p string) bool {
	switch p {
	case "/health/live", "/health/ready",
		"/api/auth/bootstrap", "/api/auth/login", "/api/auth/refresh", "/api/auth/logout":
		return true
	}
	return false
}

// TestConstitutionalPrincipleRegistry pins the constitutional principle set to
// the runtime registry rather than to a documentation string.
//
// This used to scan api/openapi.yaml for the 18 names, because the spec carried
// an AuditEvent schema whose constitutional_principle field enumerated them.
// That schema described an audit endpoint the server never routed, so it was
// removed as dead contract; the registry in internal/principlemapping is the
// authority the audit system actually validates against, and asserting it here
// catches a missing or renamed principle rather than a missing line of YAML.
func TestConstitutionalPrincipleRegistry(t *testing.T) {
	want := []string{
		"Vision First", "Architecture Before Implementation",
		"Documentation Is Part of the Product", "Quality Over Speed",
		"Modular Design", "Replaceability", "Separation of Concerns",
		"AI Independence", "Security by Design", "Privacy by Design",
		"Human Oversight", "Continuous Improvement", "Observability",
		"Backward Compatibility", "Simplicity", "Scalability",
		"Explicit Decisions", "Founder Authority",
	}
	got := principlemapping.AllPrinciples()
	require.Len(t, got, 18, "the constitution defines exactly 18 principles")
	require.ElementsMatch(t, want, got,
		"the runtime principle registry must match the constitution")

	seen := map[string]bool{}
	for _, p := range got {
		require.False(t, seen[p], "principle %q is registered twice", p)
		seen[p] = true
	}
}

// apiURL returns the base URL of the live HTTP API (docker-network default).
func apiURL() string {
	return envOrDefault("AUSTRO_API_URL", "http://api:8080")
}

// TestHealthChecks verifies the health endpoints are reachable and that
// liveness and readiness are distinct, typed probes.
func TestHealthChecks(t *testing.T) {
	c := &http.Client{Timeout: 5 * time.Second}

	respLive, err := c.Get(apiURL() + "/health/live")
	require.NoError(t, err, "must be able to reach /health/live for the API")
	t.Cleanup(func() { respLive.Body.Close() })
	require.Equal(t, http.StatusOK, respLive.StatusCode, "/health/live must return 200")

	liveBody, err := io.ReadAll(respLive.Body)
	require.NoError(t, err)
	var live map[string]string
	require.NoError(t, json.Unmarshal(liveBody, &live))
	require.Equal(t, "ok", live["status"], "/health/live must report status=ok")

	respReady, err := c.Get(apiURL() + "/health/ready")
	require.NoError(t, err, "must be able to reach /health/ready for the API")
	t.Cleanup(func() { respReady.Body.Close() })
	require.Equal(t, http.StatusOK, respReady.StatusCode, "/health/ready must return 200")

	readyBody, err := io.ReadAll(respReady.Body)
	require.NoError(t, err)
	var ready map[string]bool
	require.NoError(t, json.Unmarshal(readyBody, &ready))
	require.True(t, ready["ready"], "/health/ready must report ready=true")

	// Liveness and readiness must be semantically distinct endpoints.
	require.NotEqual(t, string(liveBody), string(readyBody),
		"liveness and readiness must be distinct probes")
}
