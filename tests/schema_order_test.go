package austro_os_test

import (
	"os"
	"strings"
	"testing"
)

const schemaSourcePath = "../infrastructure/database/database.go"

func readSchemaSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(schemaSourcePath)
	if err != nil {
		t.Fatalf("failed to read schema source: %v", err)
	}
	return string(src)
}

func TestSchemaOrderWorkspacesBeforeCeos(t *testing.T) {
	content := readSchemaSource(t)

	tables := []string{
		"founders", "workspaces", "ceos", "departments", "teams",
		"ai_employees", "audit_events", "tasks", "knowledge_documents",
		"publications", "pipelines", "users",
	}
	for _, table := range tables {
		marker := "CREATE TABLE IF NOT EXISTS " + table + " ("
		if !strings.Contains(content, marker) {
			t.Errorf("idempotent marker missing for table %q", table)
		}
	}

	wsIdx := strings.Index(content, "CREATE TABLE IF NOT EXISTS workspaces (")
	ceosIdx := strings.Index(content, "CREATE TABLE IF NOT EXISTS ceos (")
	if wsIdx < 0 || ceosIdx < 0 {
		t.Fatal("workspaces/ceos CREATE TABLE declarations not found in schema")
	}
	if wsIdx > ceosIdx {
		t.Fatalf("workspaces is created AFTER ceos (workspaces idx=%d, ceos idx=%d)", wsIdx, ceosIdx)
	}

	ceosBlock := content[ceosIdx:]
	if end := strings.Index(ceosBlock, ");"); end > 0 {
		ceosBlock = ceosBlock[:end]
	}
	if !strings.Contains(ceosBlock, "REFERENCES workspaces(id)") {
		t.Error("ceos foreign key to workspaces(id) is missing or altered")
	}
	if strings.Contains(ceosBlock, "DEFERRABLE") {
		t.Error("ceos foreign key must remain non-deferrable")
	}
}

func TestSchemaRLSPoliciesIntact(t *testing.T) {
	content := readSchemaSource(t)

	if got := strings.Count(content, "CREATE POLICY workspace_isolation_policy"); got != 9 {
		t.Errorf("expected 9 workspace_isolation_policy declarations, got %d", got)
	}
	if !strings.Contains(content, "CREATE POLICY founder_org_policy") {
		t.Error("founder_org_policy declaration missing")
	}
	if !strings.Contains(content, "CREATE POLICY user_scope_policy") {
		t.Error("user_scope_policy declaration missing")
	}
	if !strings.Contains(content, "ENABLE ROW LEVEL SECURITY") {
		t.Error("ROW LEVEL SECURITY enable statements missing")
	}
}