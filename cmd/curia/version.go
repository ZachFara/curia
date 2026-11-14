package main

import (
	"runtime/debug"
	"strings"
)

// Release builds set these with
// -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
// Builds without them (go install, go build) take the version from the build
// information the Go toolchain embeds.
var (
	version = ""
	commit  = ""
	date    = ""
)

// shortRevision is how many characters of a commit hash are shown.
const shortRevision = 7

// resolveVersion is the string `curia --version` prints for this binary.
func resolveVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return versionString(version, commit, date, bi)
}

// versionString picks the version to print. A version stamped at link time
// wins and gets its commit and date appended. Otherwise it is the main
// module's version from bi (a tag for go install ...@v0.1.0, a pseudo-version
// for an untagged build), then the short VCS revision, then "dev". bi may be
// nil.
func versionString(ldVersion, ldCommit, ldDate string, bi *debug.BuildInfo) string {
	if ldVersion != "" {
		return stamped(ldVersion, ldCommit, ldDate)
	}
	if bi == nil {
		return "dev"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > shortRevision {
		rev = rev[:shortRevision]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// stamped is a link-time version with its commit and date in parentheses,
// leaving out whichever is empty.
func stamped(v, c, d string) string {
	if len(c) > shortRevision {
		c = c[:shortRevision]
	}
	var extra []string
	for _, s := range []string{c, d} {
		if s != "" {
			extra = append(extra, s)
		}
	}
	if len(extra) == 0 {
		return v
	}
	return v + " (" + strings.Join(extra, ", ") + ")"
}
