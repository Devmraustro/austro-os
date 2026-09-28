package buildinfo_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"austro-os/internal/buildinfo"
)

// Default development identity.
//
// A locally built binary has no release metadata. It must say so rather than
// claim a version: the whole point of the package is that "which build is this"
// has an answer, and "dev/unknown" is a true answer.

func TestDefaultsReportUnversionedDevelopmentBuild(t *testing.T) {
	require.Equal(t, buildinfo.DefaultVersion, buildinfo.Version,
		"Version must default to %q so a binary with no ldflags is visibly a dev build", buildinfo.DefaultVersion)
	require.Equal(t, buildinfo.DefaultCommit, buildinfo.Commit,
		"Commit must default to %q so a binary with no ldflags is visibly a dev build", buildinfo.DefaultCommit)
	require.True(t, buildinfo.IsDevBuild(),
		"IsDevBuild must be true when neither the version nor the commit was linked in")
}

func TestLineIsGreppableAndCarriesBothFields(t *testing.T) {
	line := buildinfo.Line("api")
	require.Contains(t, line, "austro-os api")
	require.Contains(t, line, "version="+buildinfo.Version)
	require.Contains(t, line, "commit="+buildinfo.Commit)
	// One line, so a log shipper or a grep does not have to reassemble it.
	require.NotContains(t, line, "\n")
}

func TestLineDistinguishesTheComponent(t *testing.T) {
	// The API and the worker are separate processes from one artifact. An
	// operator pasting --version output into a ticket needs to know which
	// process produced it.
	require.Contains(t, buildinfo.Line("api"), "austro-os api")
	require.Contains(t, buildinfo.Line("worker"), "austro-os worker")
	require.NotEqual(t, buildinfo.Line("api"), buildinfo.Line("worker"))
}

func TestPrintVersionNotesAnUnlinkedBuild(t *testing.T) {
	var buf bytes.Buffer
	buildinfo.PrintVersion(&buf, "worker")
	out := buf.String()
	require.Contains(t, out, "austro-os worker")
	require.Contains(t, out, "version=")
	require.Contains(t, out, "commit=")
	if buildinfo.IsDevBuild() {
		require.Contains(t, out, "development build",
			"an unlinked build must explain itself, so an operator does not have to infer why the answer looks like that")
	}
}

func TestPrintVersionOmitsTheNoteForALinkedBuild(t *testing.T) {
	// Guards the branch the defaults test cannot reach: a release build must not
	// claim to be a development build.
	restore := swap(t, "1.0", "0000000000000000000000000000000000000000")
	defer restore()
	require.False(t, buildinfo.IsDevBuild())

	var buf bytes.Buffer
	buildinfo.PrintVersion(&buf, "api")
	require.NotContains(t, buf.String(), "development build")
	require.Contains(t, buf.String(), "version=1.0")
}

// Injected release identity.
//
// The linker is not simulated here. Setting a Go package variable at runtime
// and calling that "linker injection" would test nothing about the linker: the
// real failure mode is a wrong -X path or a non-constant initialiser, and both
// are invisible to an in-process test. The actual injection is proven by
// TestReleaseBuildsReportInjectedIdentity, which runs a real binary.

// swap sets the package identity for the duration of a test and returns a
// restore function. The values are restored because they are process-global and
// a later test in this binary would otherwise see the injected value.
func swap(t *testing.T, version, commit string) func() {
	t.Helper()
	oldV, oldC := buildinfo.Version, buildinfo.Commit
	buildinfo.Version, buildinfo.Commit = version, commit
	return func() { buildinfo.Version, buildinfo.Commit = oldV, oldC }
}

func TestInjectedIdentityIsReportedNotTreatedAsDev(t *testing.T) {
	restore := swap(t, "1.0", strings.Repeat("a", 40))
	defer restore()
	require.False(t, buildinfo.IsDevBuild(),
		"a build with a linked version and commit must not be classified as a development build")
	require.Contains(t, buildinfo.Line("api"), "version=1.0")
	require.Contains(t, buildinfo.Line("api"), "commit="+strings.Repeat("a", 40))
}

// Metadata unavailable.
//
// A partially stamped build is the dangerous case: version present, commit
// missing. It must still be classified as unversioned, because a version with no
// source commit is not a traceable build.

func TestPartialMetadataIsStillADevBuild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		commit  string
	}{
		{"version only", "1.0", buildinfo.DefaultCommit},
		{"commit only", buildinfo.DefaultVersion, strings.Repeat("b", 40)},
		{"both defaults", buildinfo.DefaultVersion, buildinfo.DefaultCommit},
		{"empty commit", "1.0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := swap(t, tc.version, tc.commit)
			defer restore()
			require.True(t, buildinfo.IsDevBuild(),
				"a build missing a version or a commit must not be trusted as a release build")
		})
	}
}

func TestEmptyVersionIsNotSilentlyAccepted(t *testing.T) {
	// An empty string is falsy in the obvious `if Version != ""` check that a
	// release guard would plausibly be written against. The defaults are
	// non-empty precisely so that check cannot be fooled.
	require.NotEmpty(t, buildinfo.DefaultVersion)
	require.NotEmpty(t, buildinfo.DefaultCommit)
}

func TestVersionArgSpelling(t *testing.T) {
	for _, arg := range []string{"--version", "-version", "version"} {
		require.True(t, buildinfo.IsVersionArg(arg), "expected %q to request build identity", arg)
	}
	// Everything else must fall through to normal startup, or the API would
	// silently exit 0 instead of serving.
	for _, arg := range []string{"", "--", "-v", "ver", "versions", "--versions", "Version", "serve", "--config=x"} {
		require.False(t, buildinfo.IsVersionArg(arg), "expected %q not to request build identity", arg)
	}
}

// ---------------------------------------------------------------------------
// The real binary
// ---------------------------------------------------------------------------

// The tests above exercise the package. This one exercises a compiled binary,
// because the property that matters is not what the package returns in-process
// but what an operator gets from a file on a host. Only a real build can catch a
// linker flag that does nothing.

// releaseBuild describes one binary built with the release identity.
type releaseBuild struct {
	name   string
	binary string
	commit string
}

const releaseVersion = "1.0"

// moduleRoot locates the module root from the test's own working directory, so
// the package patterns below resolve against this module rather than whatever
// directory the runner happened to start in.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not locate go.mod from %s", dir)
		dir = parent
	}
}

// buildReleaseBinary compiles a real binary with the release ldflags and returns
// its path. Building the actual main packages is the point: a test that compiled
// a scratch program would not prove that the -X path reaches the shipped
// binaries.
//
// The build runs with Dir set to the module root and absolute package paths, so
// it compiles the shipped entrypoints and not the test's own package.
func buildReleaseBinary(t *testing.T, pkg, out, version, commit string) string {
	t.Helper()
	root := moduleRoot(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, out)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build",
		"-trimpath",
		"-ldflags", "-s -w"+
			" -X austro-os/internal/buildinfo.Version="+version+
			" -X austro-os/internal/buildinfo.Commit="+commit,
		"-o", bin, filepath.Join(root, pkg),
	)
	cmd.Dir = root
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s with injected identity failed: %v\n%s", pkg, err, combined)
	}
	require.FileExists(t, bin)
	return bin
}

func TestReleaseBuildsReportInjectedIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("compiling the release binaries is too slow for -short")
	}
	commit := strings.Repeat("c", 40)

	api := buildReleaseBinary(t, ".", "austro-api", releaseVersion, commit)
	worker := buildReleaseBinary(t, "./cmd/worker", "austro-worker", releaseVersion, commit)

	for _, tc := range []struct {
		component string
		binary    string
	}{
		{"api", api},
		{"worker", worker},
	} {
		t.Run(tc.component, func(t *testing.T) {
			out, err := exec.Command(tc.binary, "--version").CombinedOutput()
			require.NoError(t, err, "--version must succeed, got: %s", out)
			got := string(out)

			require.Contains(t, got, "austro-os "+tc.component,
				"the binary must name its component so pasted output is unambiguous")
			require.Contains(t, got, "version="+releaseVersion,
				"the linked version must be visible in the real binary")
			require.Contains(t, got, "commit="+commit,
				"the linked commit must be visible in the real binary")
			require.NotContains(t, got, "development build",
				"a binary with linked metadata must not claim to be a development build")
			require.NotContains(t, got, buildinfo.DefaultCommit,
				"the linked commit must have replaced the default, not been printed alongside it")
		})
	}
}

func TestUnlinkedBinaryReportsDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("compiling the binaries is too slow for -short")
	}
	// No ldflags at all: the local `go build` case. Built through the shared
	// helper with the defaults spelled out is not possible (it always injects),
	// so this path compiles directly from the module root.
	root := moduleRoot(t)
	dir := t.TempDir()
	api := filepath.Join(dir, "austro-api")
	if runtime.GOOS == "windows" {
		api += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", api, filepath.Join(root, "."))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "building without ldflags failed: %v\n%s", err, out)
	require.FileExists(t, api)

	res, err := exec.Command(api, "--version").CombinedOutput()
	require.NoError(t, err, "--version must succeed even with no linked metadata, got: %s", res)
	got := string(res)
	require.Contains(t, got, "version="+buildinfo.DefaultVersion)
	require.Contains(t, got, "commit="+buildinfo.DefaultCommit)
	require.Contains(t, got, "development build",
		"a binary with no linked metadata must say so")
}

func TestVersionCommandWorksWithoutAnyEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("compiling the binary is too slow for -short")
	}
	commit := strings.Repeat("d", 40)
	api := buildReleaseBinary(t, ".", "austro-api", releaseVersion, commit)

	// A deliberately hostile environment: every setting the API requires is
	// absent or wrong. Identity must still be answerable, because the operator
	// asking "which build is this" is often doing so *because* the environment
	// is broken. A version command that needed a working config would be
	// useless exactly then.
	cmd := exec.Command(api, "--version")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		// No AUSTRO_* variables at all.
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "--version must not require configuration, got: %s", out)
	require.Contains(t, string(out), "commit="+commit)
}

func TestVersionCommandDoesNotTouchInfrastructure(t *testing.T) {
	if testing.Short() {
		t.Skip("compiling the binary is too slow for -short")
	}
	api := buildReleaseBinary(t, ".", "austro-api", releaseVersion, strings.Repeat("e", 40))

	start := time.Now()
	out, err := exec.Command(api, "--version").CombinedOutput()
	elapsed := time.Since(start)
	require.NoError(t, err, "got: %s", out)
	require.Contains(t, string(out), "version="+releaseVersion)

	// LoadStrict is fail-fast, but the API would still attempt a database
	// connection and a migration before serving. If --version reached any of
	// that, it would either hang or take far longer than a print.
	require.Less(t, elapsed, 10*time.Second,
		"--version returned slowly, which suggests it is doing more than printing identity")
}
