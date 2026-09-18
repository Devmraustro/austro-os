package austro_os_test

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"austro-os/internal/api"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// specOperation is one operation declared by api/openapi.yaml.
type specOperation struct {
	method     string
	path       string
	principles bool
	secure     bool
}

// nodeHasBearerAuth reports whether a `security` node references the bearerAuth
// scheme. The scheme is looked for as a scalar anywhere in the node so that
// either the compact `- bearerAuth: []` form or a longer requirement list is
// recognised.
func nodeHasBearerAuth(n *yaml.Node) bool {
	if n.Value == "bearerAuth" {
		return true
	}
	for _, c := range n.Content {
		if nodeHasBearerAuth(c) {
			return true
		}
	}
	return false
}

// parseOpenAPI walks the spec as a node tree rather than unmarshalling into a
// map. A map silently collapses duplicate path keys, which is exactly how a
// spec can advertise an operation twice and hide one of them from review.
func parseOpenAPI(t *testing.T) (ops []specOperation, schemas map[string]bool, raw string) {
	t.Helper()
	path := filepath.Join(repoRoot(t), "api", "openapi.yaml")
	f, err := os.Open(path)
	require.NoError(t, err, "api/openapi.yaml must exist")
	defer f.Close()
	b, err := io.ReadAll(f)
	require.NoError(t, err)
	raw = string(b)

	var root yaml.Node
	require.NoError(t, yaml.Unmarshal(b, &root), "api/openapi.yaml must be valid YAML")
	require.Equal(t, yaml.DocumentNode, root.Kind)
	doc := root.Content[0]
	require.Equal(t, yaml.MappingNode, doc.Kind)

	schemas = map[string]bool{}
	var pathsNode, componentsNode *yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		switch doc.Content[i].Value {
		case "paths":
			pathsNode = doc.Content[i+1]
		case "components":
			componentsNode = doc.Content[i+1]
		}
	}
	require.NotNil(t, pathsNode, "the spec must declare paths")

	seen := map[string]string{}
	for i := 0; i+1 < len(pathsNode.Content); i += 2 {
		p := pathsNode.Content[i].Value
		item := pathsNode.Content[i+1]
		require.Equal(t, yaml.MappingNode, item.Kind, "path item for %s", p)
		if prev, dup := seen[p]; dup {
			t.Errorf("duplicate path key %q in api/openapi.yaml: it is declared more than once (%s). "+
				"YAML keeps only one of them, so the other operations silently disappear from the "+
				"published contract. Merge the methods under a single path key.", p, prev)
		}
		var methods []string
		for j := 0; j+1 < len(item.Content); j += 2 {
			methods = append(methods, item.Content[j].Value)
		}
		seen[p] = "methods: " + strings.Join(methods, ",")

		for j := 0; j+1 < len(item.Content); j += 2 {
			method := strings.ToUpper(item.Content[j].Value)
			if !isHTTPMethod(item.Content[j].Value) {
				continue
			}
			op := specOperation{method: method, path: p}
			if opNode := item.Content[j+1]; opNode.Kind == yaml.MappingNode {
				for k := 0; k+1 < len(opNode.Content); k += 2 {
					switch opNode.Content[k].Value {
					case "principles":
						op.principles = true
					case "security":
						op.secure = nodeHasBearerAuth(opNode.Content[k+1])
					}
				}
			}
			ops = append(ops, op)
		}
	}

	if componentsNode != nil && componentsNode.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(componentsNode.Content); i += 2 {
			if componentsNode.Content[i].Value != "schemas" {
				continue
			}
			s := componentsNode.Content[i+1]
			for j := 0; j+1 < len(s.Content); j += 2 {
				schemas[s.Content[j].Value] = false
			}
		}
	}
	for name := range schemas {
		if strings.Contains(raw, "#/components/schemas/"+name) {
			schemas[name] = true
		}
	}
	return ops, schemas, raw
}

// TestOpenAPIMatchesRegisteredRoutes is the contract gate. The published API
// document must describe exactly the routes the server registers: no phantom
// operations that a generated client would call and receive a 404 or 405 for,
// and no live route that the document omits.
func TestOpenAPIMatchesRegisteredRoutes(t *testing.T) {
	ops, _, _ := parseOpenAPI(t)

	declared := map[string]bool{}
	for _, op := range ops {
		declared[op.method+" "+op.path] = true
	}
	registered := map[string]bool{}
	for _, r := range api.Routes() {
		registered[r.String()] = true
	}

	var phantoms, undocumented []string
	for k := range declared {
		if !registered[k] {
			phantoms = append(phantoms, k)
		}
	}
	for k := range registered {
		if !declared[k] {
			undocumented = append(undocumented, k)
		}
	}
	sort.Strings(phantoms)
	sort.Strings(undocumented)

	require.Empty(t, phantoms,
		"api/openapi.yaml advertises operations the server does not route. A client generated "+
			"from this document would call them and be refused. Either implement the route and its "+
			"authorization rule, or remove the operation from the document: documenting behavior "+
			"that is not implemented is a contract defect, not documentation.")
	require.Empty(t, undocumented,
		"the server routes operations that api/openapi.yaml does not describe. Document them.")
}

// TestOpenAPIDeclaresPrinciplesForAuthenticatedOperations keeps the
// constitutional-principle mapping honest: every operation that is not an
// unauthenticated liveness probe must name the principle it serves.
func TestOpenAPIDeclaresPrinciplesForAuthenticatedOperations(t *testing.T) {
	ops, _, _ := parseOpenAPI(t)
	for _, op := range ops {
		if op.path == "/health/live" || op.path == "/health/ready" {
			continue
		}
		require.True(t, op.principles,
			"%s %s must declare the constitutional principles it serves", op.method, op.path)
	}
}

// TestOpenAPIRequiresBearerAuthOnProtectedOperations keeps the security
// requirement from silently disappearing. Dropping a `security:` block would
// publish the operation as unauthenticated and every other parity check would
// still pass: the route is registered and the principles are declared, but the
// contract now invites anonymous calls. Only the liveness probes and the
// pre-authentication auth endpoints may be unauthenticated.
func TestOpenAPIRequiresBearerAuthOnProtectedOperations(t *testing.T) {
	ops, _, _ := parseOpenAPI(t)
	public := map[string]bool{
		"GET /health/live":         true,
		"GET /health/ready":        true,
		"POST /api/auth/bootstrap": true,
		"POST /api/auth/login":     true,
		"POST /api/auth/refresh":   true,
		"POST /api/auth/logout":    true,
	}
	for _, op := range ops {
		key := op.method + " " + op.path
		if public[key] {
			require.False(t, op.secure,
				"%s is a public endpoint and must not advertise bearerAuth", key)
			continue
		}
		require.True(t, op.secure,
			"%s must require bearerAuth; a missing security block would publish it as unauthenticated", key)
	}
}

// TestOpenAPIHasNoOrphanSchemas catches the residue left behind when an
// operation is removed: a schema nothing references is dead contract that
// readers will assume is live.
func TestOpenAPIHasNoOrphanSchemas(t *testing.T) {
	_, schemas, _ := parseOpenAPI(t)
	var orphans []string
	for name, used := range schemas {
		if !used {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	require.Empty(t, orphans, "these schemas are declared but referenced by no operation")
}
