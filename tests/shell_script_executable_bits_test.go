package austro_os_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A tracked shell script that something executes directly must be tracked as
// executable. A file stored as mode 100644 checks out non-executable, and the
// failure lands on a Linux runner as "Permission denied" - not on the machine
// that made the mistake.
//
// scripts/build-release.sh was stored as 100644 from the very commit that added
// the workflow step calling it (native-arm64-deploy.yml runs
// `scripts/build-release.sh /tmp/native arm64` under `set -euo pipefail`, with no
// chmod). It hid on Windows because Git Bash does not enforce the stored mode bit,
// so the defect was invisible to anyone editing on this platform.
//
// Invoking a script through an interpreter (`bash name.sh`) needs no execute bit,
// and a step that chmods it first is likewise fine. Both are accepted here; only
// a bare, un-chmodded invocation is a defect.

// interpreterPrefix matches a script that is handed to a shell to run rather than
// executed directly.
var interpreterPrefix = regexp.MustCompile(`(?:^|[\s;|&({])(?:bash|sh|dash|zsh|ksh|source|\.)\s+$`)

// chmodPattern matches `chmod +x <path>` for the path captured below.
var chmodPattern = regexp.MustCompile(`chmod\s+(?:-[A-Za-z]+\s+)*(?:a\+x|\+x)\s+(\S+)`)

// executableFilesAre the directories whose contents can actually run a command.
// Documentation is deliberately excluded: it refers to scripts in prose, inside
// backticks, which is not an invocation.
var executableFilesAre = []string{
	".github/workflows",
	"scripts",
	"deploy",
	"Makefile",
}

func gitLines(t *testing.T, root string, args ...string) []string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	require.NoError(t, err, "git %v", args)
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

func TestDirectlyInvokedShellScriptsAreTrackedExecutable(t *testing.T) {
	root, err := locateRepoRoot()
	require.NoError(t, err)

	// Map every tracked shell script to its stored mode.
	modes := map[string]string{}
	for _, line := range gitLines(t, root, "ls-files", "-s", "--", "*.sh") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		modes[fields[3]] = fields[0]
	}
	require.NotEmpty(t, modes, "expected tracked shell scripts")

	// Only files that can run a command are searched for invocations.
	var candidates []string
	for _, name := range executableFilesAre {
		full := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			candidates = append(candidates, full)
			continue
		}
		entries, err := os.ReadDir(full)
		require.NoError(t, err)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			switch strings.ToLower(filepath.Ext(e.Name())) {
			case ".yml", ".yaml", ".sh", ".mk":
				candidates = append(candidates, filepath.Join(full, e.Name()))
			}
		}
	}
	require.NotEmpty(t, candidates)

	var defects []string
	for _, path := range candidates {
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		rel = filepath.ToSlash(rel)

		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(body)

		// Paths this file chmods itself are exempt.
		chmods := map[string]bool{}
		for _, m := range chmodPattern.FindAllStringSubmatch(text, -1) {
			chmods[filepath.ToSlash(strings.Trim(m[1], `"'`))] = true
		}

		lines := strings.Split(text, "\n")
		for _, script := range sortedKeys(modes) {
			quoted := regexp.QuoteMeta(script)
			// A bare invocation: start of line, or after a separator or space.
			direct := regexp.MustCompile(`(?:^|[\s;|&({])` + quoted + `(?:\s|$)`)
			// The same script run through an interpreter is not a bare invocation.
			viaShell := regexp.MustCompile(`(?:^|[\s;|&({])(?:bash|sh|dash|zsh|ksh|source|\.)\s+` + quoted + `(?:\s|$)`)

			for i, line := range lines {
				// A usage example in a comment is documentation, not an
				// invocation; scripts document themselves this way.
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if viaShell.MatchString(line) || !direct.MatchString(line) {
					continue
				}
				// Confirm the match is not simply the tail of an interpreter call
				// that spans the line, then check for a chmod of the same file.
				idx := direct.FindStringIndex(line)
				if idx != nil && idx[0] > 0 && interpreterPrefix.MatchString(line[:idx[0]+1]) {
					continue
				}
				if chmods[script] {
					continue
				}
				if modes[script] != "100755" {
					defects = append(defects,
						rel+":"+itoa(i+1)+" executes "+script+" directly, but it is tracked as mode "+modes[script])
				}
			}
		}
	}

	require.Empty(t, defects,
		"these shell scripts are executed directly but are not tracked executable, "+
			"which fails with Permission denied on a Linux runner:\n  "+strings.Join(defects, "\n  "))
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Deterministic order so a failure is reproducible.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
