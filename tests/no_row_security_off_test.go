package austro_os_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rowSecurityDirective returns the forbidden PostgreSQL directive assembled from
// fragments. This keeps the exact literal out of the detector's own source so
// the scan can never trip over itself.
func rowSecurityDirective() string {
	return "SET " + "row_security" + " = " + "off"
}

// TestNoRowSecurityOff verifies the repository never disables PostgreSQL row
// security. Per ADR-007, turning row security off is absolutely prohibited:
// workspace isolation is enforced through proper RLS policies only.
func TestNoRowSecurityOff(t *testing.T) {
	// The ADR-007 contract forbids actually disabling row security. This probe
	// flags the actionable SQL directive. Two permitted reference kinds are
	// exempt: comment lines that merely cite the rule, and any fail-fast guard
	// whose log message asserts the prohibition.
	prohibited := []string{rowSecurityDirective()}

	// Only scan text-like source and config files (skip binaries/.git, tests,
	// scripts, and the CI workflows that act as the detector).
	exts := map[string]bool{
		".go": true, ".yaml": true, ".yml": true, ".sql": true,
		".md": true, ".toml": true, ".mod": true, ".env": true,
		".sh": true, ".json": true,
	}

	var violations []string
	err := filepath.Walk(repoRoot(t), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" ||
				info.Name() == "tests" || info.Name() == "scripts" ||
				info.Name() == ".github" {
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			upper := strings.ToUpper(line)
			if !strings.Contains(upper, strings.ToUpper(prohibited[0])) {
				continue
			}
			// Exempt references that merely state the rule rather than disable
			// row security: comment lines, and any fail-fast guard whose log
			// message asserts the prohibition.
			if isCommentLine(line) || strings.Contains(line, "CRITICAL: SET row_security") {
				continue
			}
			violations = append(violations, path+" :: "+line)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("error scanning repository: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("ADR-007 violation: prohibited row-security directive (%s) found in: %v", prohibited[0], violations)
	}
}

// TestRowSecurityOffDetectorIsBounded also guards the CI workflow definition
// which contains the directive as a grep pattern. That is an allowed usage (it
// is the *detector*, not code). This test documents and constrains that the
// only located occurrences are inside the CI detector, comment lines, or a
// fail-fast guard message.
func TestRowSecurityOffDetectorIsBounded(t *testing.T) {
	// The directive may only appear inside the CI workflow that *detects* it,
	// in comment lines that cite the rule, or in a fail-fast guard message. If
	// it ever appears in Go/SQL source as a real directive, the gate fails.
	matches := grepContents(t, rowSecurityDirective())
	for _, m := range matches {
		if isCommentLine(m) ||
			strings.Contains(m, ".github/workflows/") ||
			strings.Contains(m, "CRITICAL: SET row_security") {
			continue
		}
		t.Fatalf("prohibited directive found outside the CI detector/guard: %s", m)
	}
}

// isCommentLine reports whether the given "path :: line" record (or plain
// line) is a comment/reference rather than an executable directive.
func isCommentLine(s string) bool {
	body := s
	if i := strings.LastIndex(s, " :: "); i >= 0 {
		body = s[i+4:]
	}
	return isCommentBody(strings.TrimSpace(body))
}

func isCommentBody(t string) bool {
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") ||
		strings.HasPrefix(t, "--") || strings.HasPrefix(t, "*")
}

func grepContents(t *testing.T, needle string) []string {
	exts := map[string]bool{".go": true, ".yaml": true, ".yml": true, ".sql": true, ".sh": true}
	var hits []string
	filepath.Walk(repoRoot(t), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "tests" || info.Name() == "scripts" {
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, needle) {
				hits = append(hits, path+" :: "+line)
			}
		}
		return nil
	})
	return hits
}