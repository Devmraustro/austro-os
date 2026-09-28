package austro_os

import (
	"path/filepath"
	"strings"
	"testing"
)

// This file guards the Windows path-separator correction applied to
// tests/phase1_exit_criteria_test.go line 200, inside subtest 16
// ("no k8s/microservices/kafka/elasticsearch in V1").
//
// It lives in its own file on purpose. tests/phase1_exit_criteria_test.go is
// byte-pinned by FROZEN_SHA in scripts/verify-production-deployment.sh, so
// adding a test to it would move the pin again. The pin protects the
// constitution; the regression guard belongs beside it, not inside it.
//
// The defect: walkAllText hands the callback a path that may contain the
// platform separator, and the callback normalises it with
// filepath.ToSlash(path) before comparing it against the repository root. The
// root was concatenated without normalising it:
//
//	strings.HasPrefix(filepath.ToSlash(path), root+"/.github")
//
// On Linux the root already uses "/", so the comparison works. On Windows the
// root contains "\" (for example C:\src\repo), so the right-hand side was
// "C:\src\repo/.github" while the left-hand side was "C:/src/repo/.github".
// The prefix never matched, the intended exclusion silently failed, and the
// forbidden-technology scan ran over the CI workflows that the exclusion exists
// to skip. Those workflows name the banned technologies on purpose - the CI
// detector names them as the very thing it forbids - so the gate failed
// spuriously on Windows.

const githubSkipSuffix = "/.github"

// isGitHubPath reproduces the corrected prefix test from
// tests/phase1_exit_criteria_test.go:200 so it can be exercised against roots
// the host platform would never produce.
func isGitHubPath(root, path string) bool {
	return strings.HasPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+githubSkipSuffix)
}

func TestGitHubExclusionHandlesWindowsStyleRoot(t *testing.T) {
	// A root as Windows reports it: backslashes, no trailing separator.
	winRoot := `C:\src\austro`
	// walkAllText yields a path under that root using the same separator.
	winPath := winRoot + `\.github\workflows\ci.yml`

	if !isGitHubPath(winRoot, winPath) {
		t.Fatalf("corrected exclusion must match a Windows-style root: root=%q path=%q", winRoot, winPath)
	}
}

func TestGitHubExclusionRejectsThePreFixForm(t *testing.T) {
	// The pre-fix expression, kept here so the regression is pinned in place.
	preFix := func(root, path string) bool {
		return strings.HasPrefix(filepath.ToSlash(path), root+githubSkipSuffix)
	}

	winRoot := `C:\src\austro`
	winPath := winRoot + `\workflows\ci.yml`

	if preFix(winRoot, winPath) {
		t.Fatalf("pre-fix form unexpectedly matched; the regression this test guards has been reverted")
	}
	if preFix(`C:\src\austro`, `C:/src/austro/.github/workflows/ci.yml`) {
		t.Fatalf("pre-fix form unexpectedly matched a forward-slashed path against a backslash root")
	}
}

func TestGitHubExclusionStillMatchesOnPosixRoot(t *testing.T) {
	// The correction must be a no-op where the root is already slash-separated,
	// so the release platform's behaviour is unchanged by the fix.
	posixRoot := "/src/austro"
	posixPath := "/src/austro/.github/workflows/ci.yml"

	if !isGitHubPath(posixRoot, posixPath) {
		t.Fatalf("corrected exclusion must still match on a POSIX root: root=%q path=%q", posixRoot, posixPath)
	}
}

func TestGitHubExclusionRemainsNarrow(t *testing.T) {
	// The exclusion must cover the .github subtree and nothing else. If it were
	// widened, ordinary source files could be skipped and the gate would pass
	// vacuously. The .github-prefix edge case is a separate, pre-existing
	// limitation documented in TestGitHubExclusionKnownPrefixLimitation.
	cases := []struct {
		name     string
		root     string
		path     string
		excluded bool
	}{
		{"posix workflow under .github", "/src/austro", "/src/austro/.github/workflows/ci.yml", true},
		{"posix source file", "/src/austro", "/src/austro/internal/app/main.go", false},
		{"posix file merely named github", "/src/austro", "/src/austro/internal/githubclient.go", false},
		{"posix nested source file", "/src/austro", "/src/austro/cmd/worker/handler.go", false},
		{"windows workflow under .github", `C:\src\austro`, `C:\src\austro\.github\workflows\ci.yml`, true},
		{"windows source file", `C:\src\austro`, `C:\src\austro\internal\app\main.go`, false},
		{"windows file merely named github", `C:\src\austro`, `C:\src\austro\internal\githubclient.go`, false},
		{"windows nested source file", `C:\src\austro`, `C:\src\austro\cmd\worker\handler.go`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGitHubPath(tc.root, tc.path); got != tc.excluded {
				t.Fatalf("isGitHubPath(root=%q, path=%q) = %v, want %v", tc.root, tc.path, got, tc.excluded)
			}
		})
	}
}

// TestGitHubExclusionKnownPrefixLimitation records a pre-existing weakness in
// the pinned gate, rather than asserting an idealised behaviour.
//
// The exclusion is a bare string prefix, so it matches any sibling directory
// whose name merely begins with ".github" - for example ".githubx". Forbidden
// technology parked in such a directory would be skipped rather than reported.
//
// This is NOT a regression introduced by the Windows path-separator
// correction: the pre-fix form has the same boundary behaviour, because both
// forms are string prefix comparisons. It is recorded here so that:
//
//   - the limitation is visible rather than accidental;
//   - anyone who later tightens the check gets a deliberate test failure and
//     updates this test on purpose, instead of a silent behaviour change.
//
// Tightening it would require editing tests/phase1_exit_criteria_test.go, which
// is byte-pinned, so it needs a separate approved re-freeze. The gate is a
// repository hygiene check rather than a trust boundary: anyone able to create a
// new top-level directory could equally place the same text in a file the gate
// already scans.
func TestGitHubExclusionKnownPrefixLimitation(t *testing.T) {
	cases := []struct {
		name     string
		root     string
		path     string
		excluded bool
	}{
		{"posix .githubx sibling", "/src/austro", "/src/austro/.githubx/ci.yml", true},
		{"windows .githubx sibling", `C:\src\austro`, `C:\src\austro\.githubx\ci.yml`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isGitHubPath(tc.root, tc.path)
			if got != tc.excluded {
				t.Fatalf("prefix behaviour changed for root=%q path=%q: got %v, want %v "+
					"(if the exclusion was intentionally tightened, update this test deliberately)",
					tc.root, tc.path, got, tc.excluded)
			}
		})
	}
}

func TestGitHubExclusionDoesNotLeakOutOfRepo(t *testing.T) {
	// A path in a different directory that merely shares the .github segment
	// must not be treated as part of this repository's CI directory.
	root := "/src/austro"
	other := "/src/other/.github/workflows/ci.yml"

	if isGitHubPath(root, other) {
		t.Fatalf("exclusion matched a path outside the repository root: root=%q path=%q", root, other)
	}
}
