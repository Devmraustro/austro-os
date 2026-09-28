// Package buildinfo carries the build identity of a compiled binary.
//
// Version and Commit are linker-settable so a released binary can report its own
// provenance instead of relying on an external manifest. The defaults are the
// safe, honest ones: a binary built without the release ldflags reports "dev"
// and "unknown" and says so, rather than silently claiming a release version it
// does not have.
//
// The two variables MUST be initialised from constant string literals. The Go
// linker only overrides a string variable whose initialiser is a constant; a
// value computed at init time is silently ignored by -X, which would make a
// correct-looking build command produce a binary with the wrong identity.
//
// Nothing here reads the environment, a file, or the network. Build identity is
// a property of the binary, and the only inputs are the values the linker sets.
package buildinfo

import (
	"fmt"
	"io"
)

// DefaultVersion and DefaultCommit are the values compiled in when no release
// metadata is linked. IsDevBuild compares against them.
const (
	DefaultVersion = "dev"
	DefaultCommit  = "unknown"
)

// Version is the release version, for example "1.0".
//
// Release builds set it with:
//
//	-ldflags "-X austro-os/internal/buildinfo.Version=1.0"
var Version = DefaultVersion

// Commit is the full lowercase hex source commit the binary was built from, for
// example "e7b6318d531144ed4f548b44708646554816b208".
//
// Release builds set it with:
//
//	-ldflags "-X austro-os/internal/buildinfo.Commit=$(git rev-parse HEAD)"
var Commit = DefaultCommit

// IsDevBuild reports whether this binary carries no linked release metadata.
//
// A caller that needs to refuse a production start on an unversioned binary
// should test this; comparing Version to a specific release string would make
// every new release require a code change.
//
// Empty counts as unlinked as well as the default. A build script that resolved
// the commit to an empty string would otherwise produce a binary that claims a
// release version and no source at all, which is the worst case: it looks
// stamped.
func IsDevBuild() bool {
	return Version == "" || Commit == "" ||
		Version == DefaultVersion || Commit == DefaultCommit
}

// IsVersionArg reports whether arg is a request for build identity.
//
// Both entrypoints accept the same spellings so an operator does not have to
// remember which binary takes which form. Only the first argument is examined;
// the build-identity subcommand takes no options.
func IsVersionArg(arg string) bool {
	switch arg {
	case "--version", "-version", "version":
		return true
	}
	return false
}

// Line renders the identity as a single greppable line for the given component,
// which is "api" or "worker".
//
// The format is stable: key=value pairs, one line, no timestamp. A build date is
// deliberately absent, because it would make two builds of the same commit differ
// and break the byte-for-byte reproducibility this project relies on. Provenance
// that must be recorded per build belongs in the release manifest, which is
// written outside the repository.
func Line(component string) string {
	return fmt.Sprintf("austro-os %s version=%s commit=%s", component, Version, Commit)
}

// PrintVersion writes the identity to w.
//
// It also writes an explicit note for an unversioned build so that an operator
// who runs --version on a locally built binary is told why the answer looks like
// that, instead of having to infer it.
func PrintVersion(w io.Writer, component string) {
	fmt.Fprintln(w, Line(component))
	if IsDevBuild() {
		fmt.Fprintln(w, "note: development build; no release metadata was linked in at build time")
	}
}
