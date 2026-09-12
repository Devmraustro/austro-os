package austro_os_test

import (
	"os"
	"path/filepath"
	"regexp"
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

// workflowExcludedFiles returns the unique --exclude targets referenced anywhere
// in the given workflow content, in first-seen order. Values are extracted
// regardless of whether the workflow quotes them (as the Phase-1 gate does).
func workflowExcludedFiles(workflow string) []string {
	re := regexp.MustCompile(`--exclude=("?)([^"\s]+)`)
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(workflow, -1) {
		name := m[2]
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// TestPhase1CoverageRemainsWired verifies the security property of the Phase-1
// workflow: it scans raw *.go/*.py text, exempts exactly ONE source file (the
// frozen Phase-1 gate), and never broadens that exemption to directories or
// other files. Multiple scan invocations may legitimately reference the same
// single exclusion.
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

	// 1. The Phase-1 detector scans raw *.go and *.py text.
	for _, marker := range []string{`--include="*.go"`, `--include="*.py"`, "grep -r"} {
		if !strings.Contains(workflow, marker) {
			t.Fatalf("Phase-1 workflow no longer scans with %s", marker)
		}
	}

	// 5. The scan covers the whole tree (infrastructure and tests included):
	// file-level exemptions are allowed, directory-wide ones never are.
	if strings.Contains(workflow, "--exclude-dir") {
		t.Fatalf("Phase-1 workflow contains a directory-wide exclusion")
	}

	// 2. The ONLY exempt source file is the frozen Phase-1 gate.
	excluded := workflowExcludedFiles(workflow)
	if len(excluded) != 1 {
		t.Fatalf("Phase-1 workflow exempts %d unique files; want exactly the frozen gate: %v", len(excluded), excluded)
	}
	if excluded[0] != "phase1_exit_criteria_test.go" {
		t.Fatalf("Phase-1 workflow exempts %q; want exactly phase1_exit_criteria_test.go", excluded[0])
	}

	// 3. Every prohibited-SQL grep run carries that same single exclusion, so
	// structurally the same file is exempted from each scan pass.
	keyword := "SET " + "row_security"
	runs := 0
	for _, line := range strings.Split(workflow, "\n") {
		if strings.Contains(line, "grep") && strings.Contains(line, keyword) {
			runs++
			if !strings.Contains(line, "--exclude=\""+excluded[0]+"\"") {
				t.Fatalf("prohibited-SQL grep run lacks the frozen-gate exclusion: %s", strings.TrimSpace(line))
			}
		}
	}
	if runs == 0 {
		t.Fatalf("Phase-1 workflow no longer runs the prohibited-SQL grep")
	}
}
