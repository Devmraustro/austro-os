package austro_os_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoRowSecurityOff verifies the repository never disables PostgreSQL row
// security. Per ADR-007, `SET row_security = off` is absolutely prohibited:
// workspace isolation is enforced through proper RLS policies only.
func TestNoRowSecurityOff(t *testing.T) {
	// The ADR-007 contract forbids actually disabling row security. This probe
	// flags the actionable SQL directive. Two permitted references are exempt:
	// (1) the CI detector that greps for the string to fail the build, and
	// (2) the postgres.go fail-fast guard whose log.Message states the rule
	//     ("CRITICAL: SET row_security = off is absolutely prohibited") — that
	//     is a safety assertion, not a disabling statement.
	prohibited := []string{"SET row_security = off"}

	// Only scan text-like source and config files (skip binaries/.git, tests,
	// scripts, and the CI detector which legitimately grep for the pattern).
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
			// row security: comment lines, and the postgres.go fail-fast guard
			// whose log message asserts the prohibition.
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
		t.Fatalf("ADR-007 violation: SET row_security = off found in: %v", violations)
	}
}

// TestNoRowSecurityOffInWorkflow also guards the CI workflow definition which
// contains a literal "SET row_security = off" string as a grep pattern. That is
// an allowed usage (it is the *detector*, not code). This test documents and
// constrains that the only located occurrences are in the detector itself and
// the fail-fast guard message.
func TestRowSecurityOffDetectorIsBounded(t *testing.T) {
	// The prohibited string may only appear inside the CI workflow that
	// *detects* it, and the postgres.go fail-fast guard message. If it ever
	// appears in Go/SQL source as a real directive, the gate must fail.
	matches := grepContents(t, "SET row_security = off")
	for _, m := range matches {
		if isCommentLine(m) ||
			strings.Contains(m, ".github/workflows/") ||
			strings.Contains(m, "CRITICAL: SET row_security") {
			continue
		}
		t.Fatalf("prohibited string found outside the CI detector/guard: %s", m)
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
