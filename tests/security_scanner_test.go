package austro_os_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// security_scanner_test.go regression-tests the Phase-1 security scanner. Every
// reference to the forbidden directive is assembled from fragments (see
// rowSecurityDirective) so this file can never be flagged by the scanner, by
// the CI grep over *.go, or by TestRowSecurityOffDetectorIsBounded.

// TestScannerSourceDoesNotSelfMatch verifies the detector source itself never
// embeds the literal directive it is meant to find.
func TestScannerSourceDoesNotSelfMatch(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "tests", "no_row_security_off_test.go"))
	if err != nil {
		t.Fatalf("read scanner source: %v", err)
	}
	if strings.Contains(string(src), rowSecurityDirective()) {
		t.Fatalf("scanner source contains the literal directive it must detect")
	}
}

// TestScannerDetectsExecutableSQL verifies real executable forbidden SQL in the
// scan domain is still reported by the same walking/grep machinery the detector
// uses.
func TestScannerDetectsExecutableSQL(t *testing.T) {
	root := repoRoot(t)
	probe := filepath.Join(root, "infrastructure", "zz_scan_probe_tmp_test.go")
	if _, err := os.Stat(probe); err == nil {
		t.Fatalf("refusing to overwrite pre-existing probe file: %s", probe)
	}
	content := "package infrastructure\n\n// temporary probe written by security_scanner_test\nconst _probeSQL = \"" + rowSecurityDirective() + "\"\n"
	if err := os.WriteFile(probe, []byte(content), 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	t.Cleanup(func() { os.Remove(probe) })

	found := false
	for _, hit := range grepContents(t, rowSecurityDirective()) {
		if strings.Contains(hit, probe) {
			if isCommentLine(hit) {
				t.Fatalf("executable probe was misclassified as a comment: %s", hit)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("scanner did not report executable forbidden SQL in %s", probe)
	}
}

// TestScannerIgnoresCommentOnlyReferences verifies harmless prose that cites the
// directive (the false-positive category behind this fix) is exempted by the
// detector's comment handling.
func TestScannerIgnoresCommentOnlyReferences(t *testing.T) {
	root := repoRoot(t)
	probe := filepath.Join(root, "infrastructure", "zz_comment_probe_tmp_test.go")
	if _, err := os.Stat(probe); err == nil {
		t.Fatalf("refusing to overwrite pre-existing probe file: %s", probe)
	}
	content := "package infrastructure\n\n// Temporary probe: the directive (" + rowSecurityDirective() + ") is banned by ADR-007.\n"
	if err := os.WriteFile(probe, []byte(content), 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	t.Cleanup(func() { os.Remove(probe) })

	var matched string
	for _, hit := range grepContents(t, rowSecurityDirective()) {
		if strings.Contains(hit, probe) {
			matched = hit
			break
		}
	}
	if matched == "" {
		t.Fatalf("comment probe was not discovered by the raw scan")
	}
	if !isCommentLine(matched) {
		t.Fatalf("comment-only reference triggered a violation verdict: %s", matched)
	}
}

// TestInfrastructurePostgresIsClean verifies the infrastructure layer contains
// no occurrence of the forbidden directive, executable or referenced.
func TestInfrastructurePostgresIsClean(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "infrastructure", "postgres", "postgres.go"))
	if err != nil {
		t.Fatalf("read postgres.go: %v", err)
	}
	s := string(src)
	if strings.Contains(s, rowSecurityDirective()) {
		t.Fatalf("infrastructure/postgres.go still contains the forbidden directive")
	}
	if !strings.Contains(s, "CREATE POLICY workspace_isolation_policy") {
		t.Fatalf("RLS policy setup is no longer wired in postgres.go")
	}
}

// TestPhase1CoverageRemainsWired verifies the existing Phase-1 security
// coverage is intact: the frozen gate still invokes the scanner, and the CI
// workflow still greps for the directive while excluding only the frozen gate
// file whose test description legitimately names it.
func TestPhase1CoverageRemainsWired(t *testing.T) {
	root := repoRoot(t)

	frozen, err := os.ReadFile(filepath.Join(root, "tests", "phase1_exit_criteria_test.go"))
	if err != nil {
		t.Fatalf("read frozen gate: %v", err)
	}
	gateSrc := string(frozen)
	if !strings.Contains(gateSrc, "TestNoRowSecurityOff(t)") {
		t.Fatalf("frozen gate no longer invokes the row-security scanner")
	}
	if !strings.Contains(gateSrc, "01 no "+rowSecurityDirective()) {
		t.Fatalf("frozen gate subtest naming the directive has changed")
	}

	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "phase1-exit-criteria.yml"))
	if err != nil {
		t.Fatalf("read Phase-1 workflow: %v", err)
	}
	workflow := string(wf)
	if !strings.Contains(workflow, "--exclude=phase1_exit_criteria_test.go") {
		t.Fatalf("Phase-1 workflow no longer excludes only the frozen gate file")
	}
	if !strings.Contains(workflow, "grep -r") {
		t.Fatalf("Phase-1 workflow no longer runs the security grep")
	}
}