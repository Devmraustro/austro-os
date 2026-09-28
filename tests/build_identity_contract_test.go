package austro_os_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"austro-os/internal/buildinfo"
)

// Build identity must be stamped into the real release build commands, not only
// into a helper nobody calls. The failure this guards against is quiet: a
// release build that omits the -X flags still compiles, still passes every
// functional test, and still produces four working binaries. Nothing fails
// until someone needs to answer "which commit is on this host" and the binary
// says dev/unknown. So the build definitions themselves are asserted here.
//
// This file also enforces the rules that keep identity meaningful: a full commit
// SHA, no build timestamp, and one place that decides the version.

// locateRepoRoot walks up from the working directory to the module root. It is
// separated from its *testing.T wrapper so the shared binary build can run once
// in a sync.Once, where reporting per-test failures is not available.
func locateRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}

func repoRootForIdentity(t *testing.T) string {
	t.Helper()
	root, err := locateRepoRoot()
	require.NoError(t, err)
	return root
}

// buildDefinition is one authoritative place where a released binary is produced.
type buildDefinition struct {
	name string
	path string
	// delegatesToScript marks a build path that produces its binaries by
	// calling the shared build script rather than running go build itself. Such
	// a path is *expected* to contain no ldflags and no -trimpath: that is the
	// whole point of a single source of truth. Its identity is verified
	// indirectly, by asserting the delegation itself.
	delegatesToScript bool
}

// buildDefinitionSelfContained lists the paths that run `go build` themselves and
// therefore must state the identity flags inline.
var buildDefinitionSelfContained = []buildDefinition{
	// The container image build. Runs go build itself.
	{name: "Dockerfile", path: "Dockerfile"},
	// The x86_64 release artifacts uploaded by phase2-ci.
	{name: "phase2-ci.yml", path: ".github/workflows/phase2-ci.yml"},
	// The shared script that everything else may delegate to.
	{name: "build-release.sh", path: "scripts/build-release.sh"},
}

// buildDefinitionDelegating lists the paths whose build is delegated.
var buildDefinitionDelegating = []buildDefinition{
	{name: "native-arm64-deploy.yml", path: ".github/workflows/native-arm64-deploy.yml", delegatesToScript: true},
}

// releaseBuildDefinitions is every path, for the checks that apply to all of them.
func allBuildDefinitions() []buildDefinition {
	out := make([]buildDefinition, 0, len(buildDefinitionSelfContained)+len(buildDefinitionDelegating))
	out = append(out, buildDefinitionSelfContained...)
	return append(out, buildDefinitionDelegating...)
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	require.NoError(t, err, "reading %s", rel)
	return string(data)
}

const identityPkgPath = "austro-os/internal/buildinfo"

// pkgVarAssignment finds a shell variable that holds the identity package path,
// e.g. `PKG=austro-os/internal/buildinfo` or `PKG="austro-os/internal/buildinfo"`.
// It returns the variable name, or "" if the path is not assigned to a variable.
//
// The trailing \r? matters: this repository is checked out with CRLF on Windows,
// and a $ that does not tolerate \r would make the check pass in CI and fail on
// the developer's machine.
var pkgVarAssignment = regexp.MustCompile(
	`(?m)^[ \t]*([A-Za-z_][A-Za-z0-9_]*)=(?:"` + regexp.QuoteMeta(identityPkgPath) + `"|` + regexp.QuoteMeta(identityPkgPath) + `)[ \t]*\r?$`)

// linksIdentityField reports whether src injects the named buildinfo field
// (Version or Commit) into a linked binary, resolving the indirection used by
// the shell and YAML build definitions.
//
// Three forms are accepted, all of which appear in this repository:
//
//	-X austro-os/internal/buildinfo.Version=1.0   (Dockerfile, literal)
//	-X ${PKG}.Version=1.0                        (build-release.sh, braced)
//	-X $PKG.Version=1.0                           (phase2-ci.yml, unbraced)
//
// A file that uses the variable form must also actually assign the variable, or
// the linker would silently receive an empty symbol name and no metadata at all.
func linksIdentityField(src, field string) bool {
	if strings.Contains(src, identityPkgPath+"."+field) {
		return true
	}
	m := pkgVarAssignment.FindStringSubmatch(src)
	if m == nil {
		return false
	}
	v := m[1]
	return strings.Contains(src, "-X ${"+v+"}."+field) ||
		strings.Contains(src, "-X $"+v+"."+field)
}

// Every self-contained release build must link both fields. -X only takes effect
// on a string variable initialised from a constant, so this also confirms the
// target package still exists at the path the build assumes.
func TestEveryReleaseBuildLinksBuildIdentity(t *testing.T) {
	root := repoRootForIdentity(t)
	for _, def := range buildDefinitionSelfContained {
		t.Run(def.name, func(t *testing.T) {
			src := readFile(t, root, def.path)
			for _, field := range []string{"Version", "Commit"} {
				require.True(t, linksIdentityField(src, field),
					"%s builds a released binary but never links buildinfo.%s; the binary would report "+
						"the default and its provenance would be unprovable", def.name, field)
			}
			// If the path is only present through a variable, the variable must
			// really be assigned. A dangling ${PKG} injects nothing.
			if !strings.Contains(src, identityPkgPath+".Version") {
				require.NotNil(t, pkgVarAssignment.FindStringSubmatch(src),
					"%s refers to the identity package through a variable but never assigns it", def.name)
			}
		})
	}
}

// A delegating build must still end up with the identity, which it gets by
// calling the shared script. It must not also inline its own flags, because two
// definitions of the same identity is how they drift apart.
func TestDelegatingBuildsDelegateRatherThanDuplicate(t *testing.T) {
	root := repoRootForIdentity(t)
	for _, def := range buildDefinitionDelegating {
		t.Run(def.name, func(t *testing.T) {
			src := readFile(t, root, def.path)
			require.Contains(t, src, "scripts/build-release.sh",
				"%s must build through the shared script so build identity has one definition", def.name)
			require.NotContains(t, src, "austro-os/internal/buildinfo.",
				"%s delegates its build but also inlines identity flags; the two definitions can drift", def.name)
			require.NotContains(t, src, "go build",
				"%s delegates its build but still calls go build directly", def.name)
		})
	}
}

// A timestamp in the linked identity would make two builds of the same commit
// differ, which is the property this whole change is built on. This applies to
// every build path, delegating or not, and must recognise both the literal and
// the variable-indirect form.
func TestNoBuildTimestampIsLinked(t *testing.T) {
	root := repoRootForIdentity(t)
	for _, def := range allBuildDefinitions() {
		t.Run(def.name, func(t *testing.T) {
			src := readFile(t, root, def.path)
			for _, field := range []string{"date", "time", "timestamp", "built"} {
				require.False(t, linksIdentityField(src, field),
					"%s links buildinfo.%s into the binary; two builds of one commit would no longer "+
						"be byte-identical, which destroys reproducibility", def.name, field)
			}
		})
	}
}

// Reproducibility depends on -trimpath. Without it the binary records the
// directory it was built in and the same commit yields different bytes.
func TestEveryReleaseBuildUsesTrimpath(t *testing.T) {
	root := repoRootForIdentity(t)
	// Only the self-contained paths: a delegating path inherits -trimpath from
	// the script it calls.
	for _, def := range buildDefinitionSelfContained {
		t.Run(def.name, func(t *testing.T) {
			src := readFile(t, root, def.path)
			require.Contains(t, src, "-trimpath",
				"%s builds a release binary without -trimpath; the build would not be reproducible", def.name)
		})
	}
}

// The native job delegates to the shared script. This is covered in detail by
// TestDelegatingBuildsDelegateRatherThanDuplicate; kept here as a named
// statement of the intent, because losing the delegation is a silent regression
// that no other test would describe.

// A release build must not silently stamp a commit onto a dirty tree. That is
// the provenance lie the package exists to prevent: a binary reporting a commit
// that does not describe the source it was compiled from.
func TestSharedBuildScriptRefusesADirtyTree(t *testing.T) {
	root := repoRootForIdentity(t)
	src := readFile(t, root, "scripts/build-release.sh")
	require.Contains(t, src, "git status --porcelain",
		"the release build must check for a dirty tree before stamping a commit")
	require.Contains(t, src, "ALLOW_DIRTY",
		"a dirty-tree refusal needs an explicit override for local builds")
	// The override must be opt-in, never the default: the guard has to be an
	// exact "${ALLOW_DIRTY:-0}" style test against "1".
	require.Regexp(t, `\$\{ALLOW_DIRTY:-0\}"?\s*!=\s*"1"`, src,
		"the dirty-tree guard must default ALLOW_DIRTY to 0 (refuse) and only proceed when it is exactly 1")
}

// Untracked files are ignored by the guard on purpose: a local scratch build must
// not be blocked by a stray editor temp file, but a *tracked* modification must
// never be stamped as a release.
func TestDirtyGuardIgnoresUntrackedButNotTrackedChanges(t *testing.T) {
	root := repoRootForIdentity(t)
	src := readFile(t, root, "scripts/build-release.sh")
	require.Contains(t, src, "--untracked-files=no",
		"the dirty guard must ignore untracked files; only tracked modifications invalidate provenance")
}

// The image build has no .git, so the commit must arrive as a build argument and
// the resulting binaries must be checked. A default of "unknown" keeps a
// forgotten --build-arg visible in the binary rather than silently wrong.
func TestDockerImageTakesCommitAsABuildArgAndVerifiesIt(t *testing.T) {
	root := repoRootForIdentity(t)
	df := readFile(t, root, "Dockerfile")
	require.Contains(t, df, "ARG COMMIT=",
		"the image build has no git, so the commit must be supplied as a build arg")
	require.Contains(t, df, `ARG COMMIT="unknown"`,
		"the default must be the honest 'unknown', so a forgotten build arg is visible in the binary")
	require.Contains(t, df, "does not embed COMMIT=",
		"the image build must fail if the binaries do not carry the requested commit")
	// Both binaries, not just one.
	require.Equal(t, 2, strings.Count(df, "does not embed COMMIT="),
		"both the API and the worker binary must be checked in the image build")
	// The labels must reflect the same values the binaries carry.
	require.Contains(t, df, `org.opencontainers.image.revision="${COMMIT}"`,
		"the image label must record the source revision the binaries were built from")
}

// An ARG is scoped to the stage that declares it. This one caught a real
// regression: the revision label was written in the runtime stage, which had no
// ARG of its own, so it expanded to the empty string. The image then claimed
// revision="" while its binaries correctly reported a commit -- exactly the
// disagreement this change exists to eliminate, reintroduced through the labels.
func TestDockerRuntimeStageRedeclaresIdentityArgs(t *testing.T) {
	root := repoRootForIdentity(t)
	df := readFile(t, root, "Dockerfile")
	runtimeStage := df[strings.Index(df, "FROM alpine"):]
	require.NotEmpty(t, runtimeStage, "the Dockerfile must have a runtime stage")
	require.Contains(t, runtimeStage, `ARG VERSION=`,
		"the runtime stage declares org.opencontainers.image.version, so it needs its own ARG VERSION; "+
			"an ARG from the builder stage is not in scope here and the label would expand to empty")
	require.Contains(t, runtimeStage, `ARG COMMIT=`,
		"the runtime stage declares org.opencontainers.image.revision, so it needs its own ARG COMMIT; "+
			"an ARG from the builder stage is not in scope here and the label would expand to empty")
	// The label must come after the redeclaration, or it still expands empty.
	// Anchored to the exact LABEL form so the explanatory comment above it,
	// which also names the label, cannot satisfy the check out of order.
	const label = `org.opencontainers.image.revision="${COMMIT}"`
	require.Contains(t, runtimeStage, label, "the runtime stage must label the image with the commit")
	require.Less(t,
		strings.Index(runtimeStage, "ARG COMMIT="),
		strings.Index(runtimeStage, label),
		"ARG COMMIT must be declared before the label that uses it")
}

// The version is a poor grep target: a token like "1.0" occurs throughout a Go
// binary, so asserting on it can never fail. A check that cannot fail is worse
// than no check, because it is read as assurance. The discriminating assertion
// is the 40-character commit; the version is asserted by executing --version.
func TestDockerAssertsOnlyOnTheDiscriminatingIdentityField(t *testing.T) {
	root := repoRootForIdentity(t)
	df := readFile(t, root, "Dockerfile")
	require.NotContains(t, df, "does not embed VERSION=",
		"grepping a Go binary for a short version token cannot fail; assert the commit in the image "+
			"and the version by running --version")
}

// Comment lines inside a continued RUN are a Dockerfile parse hazard: the parser
// strips or rejects them depending on the frontend version. The identity
// verification must not live there.
func TestDockerHasNoCommentInsideAContinuedRun(t *testing.T) {
	root := repoRootForIdentity(t)
	df := readFile(t, root, "Dockerfile")
	lines := strings.Split(strings.ReplaceAll(df, "\r\n", "\n"), "\n")
	inContinuedRun := false
	for i, line := range lines {
		if inContinuedRun && strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("line %d is a comment inside a continued RUN: %q\n"+
				"comment lines are removed before instructions are executed, so this either "+
				"breaks the build or silently truncates the instruction",
				i+1, strings.TrimSpace(line))
		}
		inContinuedRun = strings.HasSuffix(strings.TrimSpace(line), `\`)
	}
}

// The Dockerfile must refuse a commit that looks plausible but is not a SHA.
// An empty value is the sharpest case: `grep -qF ""` matches everything, so an
// unchecked empty COMMIT would make the verification below it pass vacuously.
func TestDockerRefusesACommitThatIsNotAFullSHA(t *testing.T) {
	root := repoRootForIdentity(t)
	df := readFile(t, root, "Dockerfile")
	require.Contains(t, df, `^[0-9a-f]{40}$`,
		"the image build must validate that COMMIT is a full lowercase 40-character SHA")
	require.Contains(t, df, "[ -z \"$VERSION\" ]",
		"an empty VERSION must be refused rather than linked as an empty string")
}

// A commit must never be assembled from a branch name, a short SHA, or the
// literal HEAD. These are the values a well-meaning operator reaches for and
// they produce images that look stamped and cannot be traced.
func TestReleaseBuildsDoNotStampABranchNameAsTheCommit(t *testing.T) {
	root := repoRootForIdentity(t)
	for _, def := range allBuildDefinitions() {
		t.Run(def.name, func(t *testing.T) {
			src := readFile(t, root, def.path)
			// A resolved SHA is required; these patterns would inject a moving target.
			for _, bad := range []string{`Commit=main`, `Commit=HEAD`, `Commit=$(git rev-parse --short`} {
				require.NotContains(t, src, bad,
					"%s stamps %s as the commit; a release must name one exact, immutable revision", def.name, bad)
			}
		})
	}
}

// The production compose file is a real release path. If it builds the image
// without the identity args, the released binaries report dev/unknown even though
// every other path is correct.
func TestProductionComposePassesBuildIdentityToBothServices(t *testing.T) {
	root := repoRootForIdentity(t)
	compose := readFile(t, root, "docker-compose.production.yml")
	require.Contains(t, compose, "AUSTRO_BUILD_COMMIT",
		"the production compose must pass the source commit to the image build")
	require.Contains(t, compose, "AUSTRO_BUILD_VERSION",
		"the production compose must pass the release version to the image build")
	// Required, not defaulted: a missing value must abort, not default silently.
	require.Regexp(t, `\$\{AUSTRO_BUILD_COMMIT:\?`, compose,
		"AUSTRO_BUILD_COMMIT must use compose's :? form so an unset value aborts the build instead "+
			"of producing an image that reports commit=unknown")
	// Both services build the same image and must receive the same args.
	require.Equal(t, 2, strings.Count(compose, "args: *austro-build-args"),
		"both the api and the worker service build the image and both must pass the identity args")
}

// The development compose intentionally keeps the defaults so a contributor can
// run it with no setup. This pins that difference so the strictness is not
// accidentally applied (or removed) without a decision.
func TestDevelopmentComposeKeepsTheUnstampedDefault(t *testing.T) {
	root := repoRootForIdentity(t)
	compose := readFile(t, root, "docker-compose.yml")
	require.NotContains(t, compose, "AUSTRO_BUILD_COMMIT",
		"the development compose must keep working with no identity setup; the Dockerfile default "+
			"'unknown' is the honest value there, and the binaries report it")
}

// The version is decided in one place. This repository has no tags, so the
// version comes from the value already declared on the image label; the startup
// log used to hardcode the same string independently.
func TestVersionHasASingleSourceAndNoHardcodedStartupLiteral(t *testing.T) {
	root := repoRootForIdentity(t)
	// The image label default is the release version.
	df := readFile(t, root, "Dockerfile")
	require.Contains(t, df, `ARG VERSION="1.0"`,
		"the image build's default version must be the release version 1.0")

	// The API startup log must read the linked value, not a literal.
	apiMain := readFile(t, root, "main.go")
	require.Contains(t, apiMain, `With("version", buildinfo.Version)`,
		"the API startup log must report the linked version")
	require.Contains(t, apiMain, `With("commit", buildinfo.Commit)`,
		"the API startup log must report the linked commit")
	require.NotContains(t, apiMain, `With("version", "1.0")`,
		"the API startup log must not hardcode the version; that is what this change removes")

	workerMain := readFile(t, root, "cmd/worker/main.go")
	require.Contains(t, workerMain, `With("version", buildinfo.Version)`,
		"the worker startup log must report the linked version")
	require.Contains(t, workerMain, `With("commit", buildinfo.Commit)`,
		"the worker startup log must report the linked commit")
}

// The x86_64 artifact is uploaded unconditionally, so it can exist even when the
// suite failed. A consumer must still be able to identify it.
func TestX64ReleaseArtifactIsIdentityChecked(t *testing.T) {
	root := repoRootForIdentity(t)
	wf := readFile(t, root, ".github/workflows/phase2-ci.yml")
	require.Contains(t, wf, "--version",
		"the release artifact job must run the binaries' version command")
	require.Contains(t, wf, "does not report commit",
		"the job must fail when a release binary does not report the expected commit")
}

// The container image is itself a released artifact, and CI is the only place
// where the exact source revision is known for certain. It must therefore be the
// place that proves the image carries it, rather than relying on the Dockerfile
// default.
func TestCIImageBuildSuppliesAndVerifiesTheCommit(t *testing.T) {
	root := repoRootForIdentity(t)
	wf := readFile(t, root, ".github/workflows/production-deployment.yml")
	require.Contains(t, wf, "--build-arg",
		"the CI image build must pass the commit explicitly; relying on the Dockerfile default "+
			"would produce an image reporting commit=unknown")
	require.Contains(t, wf, "GITHUB_SHA",
		"the CI image build must stamp the checked-out commit")
	// The label and the binaries must be checked to agree with each other.
	require.Contains(t, wf, "org.opencontainers.image.revision",
		"CI must assert the image label matches the commit, or an out-of-scope ARG goes unnoticed")
	require.Contains(t, wf, "does not report commit=",
		"CI must execute the binaries' --version and confirm the commit they report")
}

// ---------------------------------------------------------------------------
// Executing the real binaries
// ---------------------------------------------------------------------------

// These run actual compiled binaries. The unit tests in internal/buildinfo cover
// the package logic; only these prove the -X flags reach a shipped executable
// and that the subcommand works with no environment at all.

const identityTestVersion = "1.0"

// identityMarkerCommit is a stand-in commit that cannot occur naturally, so a
// match cannot be satisfied by unrelated content in the binary.
//
// A full 40-character SHA is used deliberately: the whole point of the change is
// that a release names its exact source commit, so a test that passed with a
// short or fake-looking identifier would not exercise the real contract.
const identityMarkerCommit = "0123456789abcdef0123456789abcdef01234567"

// compiled identity binaries, built at most once for the whole package. Four
// separate go build invocations cost several minutes on a cold cache and
// proved nothing extra, so the two entrypoints are built once and shared by
// every test that needs to execute a real binary.
type identityBinary struct {
	path      string
	component string
}

var (
	identityBuildOnce sync.Once
	apiIdentityBin    string
	workerIdentityBin string
	identityBuildErr  error
)

func buildIdentityBinaries(t *testing.T) (api, worker string) {
	t.Helper()
	identityBuildOnce.Do(func() {
		apiIdentityBin, workerIdentityBin, identityBuildErr =
			compileIdentityBinaries()
	})
	require.NoError(t, identityBuildErr, "building the identity test binaries failed")
	return apiIdentityBin, workerIdentityBin
}

func compileIdentityBinaries() (api, worker string, err error) {
	root, rootErr := locateRepoRoot()
	if rootErr != nil {
		return "", "", rootErr
	}
	dir, mkErr := os.MkdirTemp("", "austro-identity-*")
	if mkErr != nil {
		return "", "", mkErr
	}
	build := func(pkg, out string) (string, error) {
		bin := filepath.Join(dir, out)
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		ldflags := "-s -w" +
			" -X " + identityPkgPath + ".Version=" + identityTestVersion +
			" -X " + identityPkgPath + ".Commit=" + identityMarkerCommit
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin, filepath.Join(root, pkg))
		cmd.Dir = root
		if out, berr := cmd.CombinedOutput(); berr != nil {
			return "", fmt.Errorf("building %s failed: %w\n%s", pkg, berr, out)
		}
		return bin, nil
	}
	if api, err = build(".", "austro-api"); err != nil {
		return "", "", err
	}
	if worker, err = build("./cmd/worker", "austro-worker"); err != nil {
		return "", "", err
	}
	return api, worker, nil
}

func TestAPIAndWorkerBinariesReportInjectedIdentity(t *testing.T) {
	api, worker := buildIdentityBinaries(t)
	for _, bin := range []identityBinary{
		{path: api, component: "api"},
		{path: worker, component: "worker"},
	} {
		t.Run(bin.component, func(t *testing.T) {
			out, err := exec.Command(bin.path, "--version").CombinedOutput()
			require.NoError(t, err, "--version failed: %s", out)
			got := string(out)
			require.Contains(t, got, "austro-os "+bin.component)
			require.Contains(t, got, "version="+identityTestVersion)
			require.Contains(t, got, "commit="+identityMarkerCommit)
			require.NotContains(t, got, "development build",
				"a binary carrying linked metadata must not report itself as a development build")
		})
	}
}

// Every accepted spelling must work on both binaries, because an operator should
// not have to know which one takes which form.
func TestBothBinariesAcceptEveryVersionSpelling(t *testing.T) {
	api, worker := buildIdentityBinaries(t)
	for _, bin := range []string{api, worker} {
		for _, arg := range []string{"--version", "-version", "version"} {
			out, err := exec.Command(bin, arg).CombinedOutput()
			require.NoError(t, err, "%s %s failed: %s", filepath.Base(bin), arg, out)
			require.Contains(t, string(out), "commit="+identityMarkerCommit,
				"%s %s did not report the linked commit", filepath.Base(bin), arg)
		}
	}
}

// The version command must work with no environment at all. A real host will not
// have DATABASE_URL, REDIS_URL or JWT_SECRET set for this purpose, so if the
// version path touched configuration validation it would fail in production and
// pass in CI where those variables are always present.
func TestVersionCommandWorksWithAnEmptyEnvironment(t *testing.T) {
	api, worker := buildIdentityBinaries(t)
	for _, bin := range []string{api, worker} {
		t.Run(filepath.Base(bin), func(t *testing.T) {
			cmd := exec.Command(bin, "--version")
			// An explicitly empty environment: no inherited variables at all.
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "--version failed with an empty environment: %s", out)
			require.Contains(t, string(out), "commit="+identityMarkerCommit)
		})
	}
}

// An unrecognised argument must not be swallowed by the version path. If it were,
// the API would exit 0 without serving and look healthy to a supervisor that only
// checks the exit code.
func TestUnknownArgumentIsNotTreatedAsVersion(t *testing.T) {
	// Uses the real implementation rather than a copy, so this cannot pass while
	// the shipped function disagrees.
	for _, arg := range []string{"--version-please", "-v", "versions", "--versions", " version"} {
		require.False(t, buildinfo.IsVersionArg(arg),
			"%q must not be treated as a version request", arg)
	}
	for _, arg := range []string{"--version", "-version", "version"} {
		require.True(t, buildinfo.IsVersionArg(arg), "%q must be accepted as a version request", arg)
	}
}

// manifestShape is the subset of release-manifest.json this suite asserts on.
// The manifest lives outside the repository by design (a file cannot contain the
// SHA of the commit that contains it), so this test locates it if present and
// otherwise reports that it was not found rather than failing.
type manifestShape struct {
	Provenance struct {
		GitCommit    string   `json:"git_commit"`
		GitTree      string   `json:"git_tree"`
		SourceMethod string   `json:"source_method"`
		BuildFlags   string   `json:"build_flags"`
		GoVersion    string   `json:"go_version"`
		WorkingTree  string   `json:"working_tree_influence"`
		ExcludedFrom []string `json:"excluded_from_source"`
	} `json:"provenance"`
	Identity struct {
		EmbeddedVersion bool   `json:"embedded_version"`
		Note            string `json:"note"`
	} `json:"identity"`
	Reproducibility struct {
		ByteIdentical bool `json:"byte_identical"`
	} `json:"reproducibility"`
	Artifacts []struct {
		Target    string `json:"target"`
		Component string `json:"component"`
		File      string `json:"file"`
		Bytes     int    `json:"bytes"`
		SHA256    string `json:"sha256"`
	} `json:"artifacts"`
}

// findManifest locates a release manifest without assuming a fixed path, so the
// suite does not hardcode a temporary directory.
func findManifest(t *testing.T) (string, bool) {
	t.Helper()
	root := repoRootForIdentity(t)
	var found string
	for _, base := range []string{
		filepath.Join(root, "release-manifest.json"),
		filepath.Join(root, "deploy", "release-manifest.json"),
	} {
		if _, err := os.Stat(base); err == nil {
			found = base
			break
		}
	}
	if found != "" {
		return found, true
	}
	// Look for one anywhere under the repo, excluding the source tree's own
	// committed files, so a locally produced candidate is still verified.
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) == "release-manifest.json" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found, found != ""
}

// If a manifest is present, it must describe the identity the binaries now
// carry. If none is present the test says so: absence is not a failure of the
// build, but a manifest that lies about identity is.
func TestManifestAgreesWithBinaryIdentity(t *testing.T) {
	path, ok := findManifest(t)
	if !ok {
		t.Skip("no release-manifest.json found; run the release build to produce one")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var m manifestShape
	require.NoError(t, json.Unmarshal(data, &m),
		"manifest at %s is not valid JSON", path)

	require.NotEmpty(t, m.Provenance.GitCommit, "manifest must name the source commit")
	require.Regexp(t, `^[0-9a-f]{40}$`, m.Provenance.GitCommit,
		"the manifest commit must be a full 40-character SHA, not an abbreviation")
	require.NotEmpty(t, m.Provenance.GitTree, "manifest must record the source tree")
	require.NotEmpty(t, m.Provenance.GoVersion, "manifest must record the toolchain")
	require.NotEmpty(t, m.Provenance.BuildFlags, "manifest must record the build flags")
	require.Contains(t, m.Provenance.SourceMethod, "git archive",
		"the source must come from a commit export, not the working tree")

	// The point of this change: the binaries now carry their own identity, so
	// the manifest must no longer claim they do not.
	require.True(t, m.Identity.EmbeddedVersion,
		"manifest still says embedded_version is false; the binaries now report their own version and commit")

	// Every artifact digest must be present and well formed.
	require.NotEmpty(t, m.Artifacts, "manifest must describe artifacts")
	for _, a := range m.Artifacts {
		require.Regexp(t, `^[0-9a-f]{64}$`, a.SHA256,
			"artifact %s has a malformed digest", a.Target)
		require.NotEmpty(t, a.Component, "artifact %s must name its component", a.Target)
		require.Positive(t, a.Bytes, "artifact %s must record a size", a.Target)
	}
}

// A manifest claiming reproducibility must have compared the artifacts.
func TestReproducibilityClaimIsBackedByTheArtifactSet(t *testing.T) {
	path, ok := findManifest(t)
	if !ok {
		t.Skip("no release-manifest.json found")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var m manifestShape
	require.NoError(t, json.Unmarshal(data, &m))
	if m.Reproducibility.ByteIdentical {
		require.NotEmpty(t, m.Artifacts,
			"a byte-identical claim with no artifacts is an unsupported claim")
	}
}

// The excluded build-host artifacts must stay excluded. This is the hygiene rule
// that keeps a crash dump and a null-filled file out of a release.
func TestExcludedArtifactsAreNotInTheCommit(t *testing.T) {
	root := repoRootForIdentity(t)
	for _, excluded := range []string{
		"erl_crash.dump",
		"specifications/Final_Technical_Specification.md",
	} {
		t.Run(filepath.Base(excluded), func(t *testing.T) {
			cmd := exec.Command("git", "cat-file", "-e", "HEAD:"+excluded)
			cmd.Dir = root
			require.Error(t, cmd.Run(),
				"%s must not be in the release commit", excluded)
		})
	}
}
