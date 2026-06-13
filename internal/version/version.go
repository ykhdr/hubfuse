// Package version exposes the resolved build/release version of the HubFuse
// binaries. A single source of truth is shared by both cmd/hubfuse and
// cmd/hubfuse-hub.
//
// GoReleaser injects release metadata into the package-level vars below via
// -ldflags -X github.com/ykhdr/hubfuse/internal/version.<var>=<value>. For
// `go install ...@vX.Y.Z` and local `go build` the vars stay empty and the
// version is recovered from debug.ReadBuildInfo().
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// set via -ldflags -X by GoReleaser; empty for go install / local builds.
var (
	version = ""
	commit  = ""
	date    = ""
)

// Info is the resolved, structured version data.
type Info struct {
	Version   string // v0.2.0 | dev-abc1234 | dev
	Commit    string // full or short sha, or "none"
	Date      string // RFC3339 build date, or "unknown"
	GoVersion string // runtime.Version()
	OSArch    string // runtime.GOOS + "/" + runtime.GOARCH
}

// ldflagsInfo carries the values injected by GoReleaser. They are passed
// explicitly into resolve so the precedence logic is testable without
// depending on the real package vars.
type ldflagsInfo struct {
	version string
	commit  string
	date    string
}

// buildData captures the subset of debug.BuildInfo that resolve needs. It is a
// plain struct so tests can drive every precedence branch deterministically
// without a real *debug.BuildInfo.
type buildData struct {
	mainVersion string // BuildInfo.Main.Version
	revision    string // vcs.revision setting
	modified    bool   // vcs.modified setting == "true"
}

const (
	develVersion   = "(devel)"
	fallbackVer    = "dev"
	noneCommit     = "none"
	unknownDate    = "unknown"
	shortSHALength = 7
)

// Get resolves the version info by wiring the real ldflags vars and
// debug.ReadBuildInfo() into the pure resolver.
func Get() Info {
	ld := ldflagsInfo{version: version, commit: commit, date: date}

	var bd buildData
	if bi, ok := debug.ReadBuildInfo(); ok {
		bd = buildDataFromBuildInfo(bi)
	}

	return resolve(ld, bd)
}

// buildDataFromBuildInfo extracts the relevant fields from a *debug.BuildInfo.
func buildDataFromBuildInfo(bi *debug.BuildInfo) buildData {
	bd := buildData{mainVersion: bi.Main.Version}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			bd.revision = s.Value
		case "vcs.modified":
			bd.modified = s.Value == "true"
		}
	}
	return bd
}

// resolve is the pure precedence helper. Order:
//  1. ldflags version set -> GoReleaser release build.
//  2. BuildInfo.Main.Version non-empty and not "(devel)" -> go install ...@vX.Y.Z.
//  3. vcs.revision present -> local build -> dev-<short-sha>(+-dirty).
//  4. bare fallback -> "dev".
func resolve(ld ldflagsInfo, bd buildData) Info {
	info := Info{
		GoVersion: runtime.Version(),
		OSArch:    runtime.GOOS + "/" + runtime.GOARCH,
	}

	switch {
	case ld.version != "":
		info.Version = ld.version
		info.Commit = firstNonEmpty(ld.commit, bd.revision, noneCommit)
		info.Date = firstNonEmpty(ld.date, unknownDate)
	case bd.mainVersion != "" && bd.mainVersion != develVersion:
		info.Version = bd.mainVersion
		info.Commit = firstNonEmpty(bd.revision, noneCommit)
		info.Date = unknownDate
	case bd.revision != "":
		info.Version = fallbackVer + "-" + shortSHA(bd.revision)
		if bd.modified {
			info.Version += "-dirty"
		}
		info.Commit = bd.revision
		info.Date = unknownDate
	default:
		info.Version = fallbackVer
		info.Commit = firstNonEmpty(bd.revision, noneCommit)
		info.Date = unknownDate
	}

	return info
}

// shortSHA trims a revision to its short (7-char) form.
func shortSHA(rev string) string {
	if len(rev) > shortSHALength {
		return rev[:shortSHALength]
	}
	return rev
}

// firstNonEmpty returns the first non-empty argument, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Short returns the single-line version string, suitable for cmd.Version and
// the --version flag.
func Short() string {
	return Get().Version
}

// Full returns a multi-line version block for the `version` subcommand.
func Full() string {
	info := Get()
	var b strings.Builder
	fmt.Fprintf(&b, "Version:    %s\n", info.Version)
	fmt.Fprintf(&b, "Commit:     %s\n", info.Commit)
	fmt.Fprintf(&b, "Date:       %s\n", info.Date)
	fmt.Fprintf(&b, "Go version: %s\n", info.GoVersion)
	fmt.Fprintf(&b, "OS/Arch:    %s", info.OSArch)
	return b.String()
}
