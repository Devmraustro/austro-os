package austro_os_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The deployment verifier depends on Python for three checks: the compose
// structural lint, the CI workflow lint and the operator-script variable audit.
// It used to probe for `python3` by name, which reports BLOCKED on a Windows host
// where CPython installs as python.exe and no python3.exe exists at all - a
// machine with working Python could not run its own release gate.
//
// These tests pin the behaviour that replaced it: each of the three names is
// accepted, an interpreter that is not Python 3 is passed over rather than
// failing a downstream check, and the absence of any interpreter is a loud
// failure rather than a quiet skip.
//
// Every case shadows all three names inside a sandbox directory placed first on
// PATH, so the result does not depend on what the host happens to have installed
// and the same four cases behave identically on Windows and on a Linux runner.

const verifierPath = "scripts/verify-production-deployment.sh"

// usableBash returns a bash that can actually execute a command. Looking it up on
// PATH is not enough: a Windows host can have a `bash` that is only a WSL
// launcher, which exists, resolves, and fails on first use.
func usableBash(t *testing.T) string {
	t.Helper()

	candidates := []string{"bash"}
	if runtime.GOOS == "windows" {
		candidates = append(candidates,
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files\Git\usr\bin\bash.exe`,
		)
	}
	for _, c := range candidates {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, "-c", "exit 0")
		if err := cmd.Run(); err == nil {
			return path
		}
	}
	t.Skip("no working bash interpreter on this host, so the shell-level checks cannot be exercised")
	return ""
}

// realPythonPath returns the absolute path of a working Python 3, asked of the
// interpreter itself rather than guessed from PATH.
func realPythonPath(t *testing.T) string {
	t.Helper()

	type probe struct {
		name string
		args []string
	}
	probes := []probe{
		{"python3", nil},
		{"python", nil},
		{"py", []string{"-3"}},
	}
	for _, p := range probes {
		path, err := exec.LookPath(p.name)
		if err != nil {
			continue
		}
		args := append(append([]string{}, p.args...), "-c", "import sys; print(sys.executable)")
		out, err := exec.Command(path, args...).Output()
		if err != nil {
			continue
		}
		exe := strings.TrimSpace(string(out))
		if exe == "" {
			continue
		}
		// Only trust it if it really is Python 3.
		check := exec.Command(exe, "-c", "import sys; raise SystemExit(0 if sys.version_info[0] >= 3 else 1)")
		if err := check.Run(); err == nil {
			return exe
		}
	}
	t.Skip("no Python 3 interpreter on this host, so the discovery cases cannot be exercised")
	return ""
}

// pythonStubKind describes how a stubbed interpreter name behaves.
type pythonStubKind int

const (
	// stubDelegates hands off to a real Python 3, so discovery must accept it.
	stubDelegates pythonStubKind = iota
	// stubRejects exists under the name but fails the version probe, the way a
	// Python 2 `python` would. Discovery must move on to the next candidate
	// rather than letting a Python 2 run a linter written with f-strings.
	stubRejects
	// stubLauncher emulates the Windows `py` launcher, which takes the requested
	// version as its own argument. Discovery has to keep that argument with the
	// command name rather than joining it into a single word.
	stubLauncher
)

// writeStub creates an executable named name in dir with the given behaviour.
func writeStub(t *testing.T, dir, name string, kind pythonStubKind, realPython string) {
	t.Helper()

	var body string
	switch kind {
	case stubDelegates:
		body = fmt.Sprintf("#!/usr/bin/env bash\nexec %q \"$@\"\n", realPython)
	case stubLauncher:
		body = fmt.Sprintf("#!/usr/bin/env bash\n[ \"$1\" = \"-3\" ] && shift\nexec %q \"$@\"\n", realPython)
	default:
		body = "#!/usr/bin/env bash\necho \"stub: not a Python 3\" >&2\nexit 1\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755))
}

// interpreterCase is one PATH configuration under test.
type interpreterCase struct {
	name    string
	stubs   map[string]pythonStubKind
	wantUse string
}

// runVerifierWithStubs installs the given stubs first on PATH and runs the real
// verifier, returning its combined output and exit code.
func runVerifierWithStubs(t *testing.T, bash, realPython string, c interpreterCase) (string, int) {
	t.Helper()

	root, err := locateRepoRoot()
	require.NoError(t, err)

	sandbox := t.TempDir()
	// Shadow every supported name, so what the host happens to have installed
	// cannot change the outcome. A name absent from the case is present but
	// non-functional, which is stricter than leaving it out and is what makes
	// the "nothing usable" case deterministic.
	for _, n := range []string{"python3", "python", "py"} {
		kind, ok := c.stubs[n]
		if !ok {
			kind = stubRejects
		}
		writeStub(t, sandbox, n, kind, realPython)
	}

	// PATH is adjusted inside bash rather than in the parent: bash owns the
	// conversion between Windows and POSIX path spellings, and guessing at that
	// in Go is how these tests end up passing for the wrong reason. `cd` plus
	// `pwd` gives a path the shell can actually use, and on a POSIX host the
	// result is already absolute and correct.
	preamble := fmt.Sprintf(`
sandbox=%s
sandbox="$(cd "$sandbox" && pwd)" || exit 90
PATH="$sandbox:$PATH"
export PATH
./%s
`, shellQuote(filepath.ToSlash(sandbox)), verifierPath)

	cmd := exec.Command(bash, "-c", preamble)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()

	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("could not run the verifier: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// shellQuote makes a value safe to embed in the double-quoted shell assignments
// above. The sandbox path comes from the OS temp directory, so it can contain
// backslashes on Windows and spaces on any host.
func shellQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// resolvedInterpreter pulls the interpreter out of the verifier's INFO line.
func resolvedInterpreter(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "INFO  Python 3 for the YAML") {
			rest := strings.TrimPrefix(line, "INFO  Python 3 for the YAML and operator-script checks: ")
			if i := strings.LastIndex(rest, " ("); i > 0 {
				return strings.TrimSpace(rest[:i])
			}
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func TestVerifierDiscoversPythonUnderEverySupportedName(t *testing.T) {
	bash := usableBash(t)
	realPython := realPythonPath(t)

	cases := []interpreterCase{
		{
			name:    "python3 is preferred when present",
			stubs:   map[string]pythonStubKind{"python3": stubDelegates},
			wantUse: "python3",
		},
		{
			name: "falls through to python when python3 is not Python 3",
			// python3 exists but fails the probe, exactly like a python2 `python`.
			stubs:   map[string]pythonStubKind{"python": stubDelegates},
			wantUse: "python",
		},
		{
			name:    "falls through to the py launcher, preserving its -3 argument",
			stubs:   map[string]pythonStubKind{"py": stubLauncher},
			wantUse: "py -3",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := runVerifierWithStubs(t, bash, realPython, c)

			require.Equal(t, 0, code,
				"the verifier must pass when a Python 3 is reachable under %q; output:\n%s", c.wantUse, out)
			require.Equal(t, c.wantUse, resolvedInterpreter(out),
				"the verifier must have used the expected interpreter; output:\n%s", out)

			// The three Python-dependent checks must actually have run.
			require.Contains(t, out, "production compose parses as structurally well-formed YAML",
				"the compose lint must run, not be skipped")
			require.Contains(t, out, "CI workflow parses as structurally well-formed YAML",
				"the workflow lint must run, not be skipped")
			require.Contains(t, out, "healthcheck reads no variable without a default",
				"the operator-script audit must run, not be skipped")
			require.NotContains(t, out, "BLOCKED",
				"no check may report BLOCKED when an interpreter was found")
		})
	}
}

func TestVerifierFailsLoudlyWhenNoPython3IsPresent(t *testing.T) {
	bash := usableBash(t)
	realPython := realPythonPath(t)

	out, code := runVerifierWithStubs(t, bash, realPython, interpreterCase{
		name:  "no interpreter at all",
		stubs: map[string]pythonStubKind{},
	})

	require.NotEqual(t, 0, code,
		"a missing interpreter must fail the gate, not pass it; output:\n%s", out)

	// Each of the three dependent checks must say so by name, and must name the
	// candidates that were tried.
	for _, want := range []string{
		"YAML structural lint could not run",
		"workflow lint could not run",
		"operator-script variable audit could not run",
		"tried: python3, python, py -3",
	} {
		require.Contains(t, out, want,
			"the failure must be explicit about what could not run; output:\n%s", out)
	}

	// And none of them may be reported as having passed.
	require.NotContains(t, out, "PASS  production compose parses as structurally well-formed YAML")
	require.NotContains(t, out, "PASS  CI workflow parses as structurally well-formed YAML")
	require.Empty(t, resolvedInterpreter(out), "no interpreter may be reported as resolved")
}

func TestVerifierKeepsTheComposeLinterNegativeControl(t *testing.T) {
	bash := usableBash(t)
	realPython := realPythonPath(t)

	out, code := runVerifierWithStubs(t, bash, realPython, interpreterCase{
		name:  "negative control still runs",
		stubs: map[string]pythonStubKind{"python3": stubDelegates},
	})

	require.Equal(t, 0, code, "output:\n%s", out)
	require.Contains(t, out, "compose linter rejects a deliberately broken file (negative control)",
		"a linter that cannot fail proves nothing; the negative control must still be exercised")
}

func TestVerifierSourceInvokesNoInterpreterByHardCodedName(t *testing.T) {
	root, err := locateRepoRoot()
	require.NoError(t, err)

	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(verifierPath)))
	require.NoError(t, err)

	// Naming python3 in the candidate list is the point; *invoking* it by a fixed
	// name is the bug, because it is the platform that decides whether that name
	// exists. So this looks for python3 in command position - at the start of a
	// command or after a shell separator - rather than merely appearing in a line.
	invocation := regexp.MustCompile(`(^|[|;&(]\s*|\$\(\s*)python3(\s|$)`)

	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if invocation.MatchString(line) {
			t.Errorf("scripts/verify-production-deployment.sh:%d invokes a hard-coded interpreter name: %s",
				i+1, trimmed)
		}
	}
	require.NotContains(t, string(src), "python3 unavailable",
		"the old misleading wording must not return")
	require.Contains(t, string(src), `PYTHON_TRIED="python3, python, py -3"`,
		"the candidates that were tried must be stated in the failure message")
}
