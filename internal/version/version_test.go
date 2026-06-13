package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name       string
		ld         ldflagsInfo
		bd         buildData
		wantVer    string
		wantCommit string
		wantDate   string
	}{
		{
			name:       "ldflags release build with all fields",
			ld:         ldflagsInfo{version: "v0.2.0", commit: "abc1234def", date: "2026-06-13T10:00:00Z"},
			bd:         buildData{mainVersion: develVersion, revision: "ignored"},
			wantVer:    "v0.2.0",
			wantCommit: "abc1234def",
			wantDate:   "2026-06-13T10:00:00Z",
		},
		{
			name:       "ldflags version set but commit/date empty -> none / unknown",
			ld:         ldflagsInfo{version: "v1.0.0"},
			bd:         buildData{revision: "deadbeefcafe"},
			wantVer:    "v1.0.0",
			wantCommit: noneCommit,
			wantDate:   unknownDate,
		},
		{
			name:       "ldflags version set, no build info commit -> none",
			ld:         ldflagsInfo{version: "v1.2.3"},
			bd:         buildData{},
			wantVer:    "v1.2.3",
			wantCommit: noneCommit,
			wantDate:   unknownDate,
		},
		{
			// `go install ...@vX.Y.Z` is built from the module cache and has
			// NO vcs.revision, so it resolves via the Main.Version branch.
			name:       "go install version without revision -> main.Version, none commit",
			ld:         ldflagsInfo{},
			bd:         buildData{mainVersion: "v0.3.1"},
			wantVer:    "v0.3.1",
			wantCommit: noneCommit,
			wantDate:   unknownDate,
		},
		{
			// Reorder guard: an in-repo `go build` stamps Main.Version with a
			// Go pseudo-version AND records vcs.revision. The vcs.revision
			// branch must win, yielding the cleaner dev-<sha> form rather than
			// the pseudo-version.
			name:       "pseudo main.Version with vcs revision -> dev-shortsha (vcs wins)",
			ld:         ldflagsInfo{},
			bd:         buildData{mainVersion: "v0.0.0-20260613100000-feedface0001", revision: "feedface0001"},
			wantVer:    "dev-feedfac",
			wantCommit: "feedface0001",
			wantDate:   unknownDate,
		},
		{
			name:       "local build with vcs revision -> dev-shortsha",
			ld:         ldflagsInfo{},
			bd:         buildData{mainVersion: develVersion, revision: "0123456789abcdef"},
			wantVer:    "dev-0123456",
			wantCommit: "0123456789abcdef",
			wantDate:   unknownDate,
		},
		{
			name:       "local build with vcs revision and modified -> dev-shortsha-dirty",
			ld:         ldflagsInfo{},
			bd:         buildData{mainVersion: develVersion, revision: "0123456789abcdef", modified: true},
			wantVer:    "dev-0123456-dirty",
			wantCommit: "0123456789abcdef",
			wantDate:   unknownDate,
		},
		{
			name:       "short revision (<= 7 chars) is not truncated",
			ld:         ldflagsInfo{},
			bd:         buildData{revision: "abc12"},
			wantVer:    "dev-abc12",
			wantCommit: "abc12",
			wantDate:   unknownDate,
		},
		{
			name:       "bare fallback - no info at all",
			ld:         ldflagsInfo{},
			bd:         buildData{},
			wantVer:    fallbackVer,
			wantCommit: noneCommit,
			wantDate:   unknownDate,
		},
		{
			name:       "devel mainVersion with no revision -> bare fallback",
			ld:         ldflagsInfo{},
			bd:         buildData{mainVersion: develVersion},
			wantVer:    fallbackVer,
			wantCommit: noneCommit,
			wantDate:   unknownDate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolve(tt.ld, tt.bd)
			assert.Equal(t, tt.wantVer, got.Version)
			assert.Equal(t, tt.wantCommit, got.Commit)
			assert.Equal(t, tt.wantDate, got.Date)
			// Runtime-derived fields are always populated.
			assert.Equal(t, runtime.Version(), got.GoVersion)
			assert.Equal(t, runtime.GOOS+"/"+runtime.GOARCH, got.OSArch)
		})
	}
}

func TestShortSHA(t *testing.T) {
	assert.Equal(t, "0123456", shortSHA("0123456789abcdef"))
	assert.Equal(t, "abc12", shortSHA("abc12"))
	assert.Equal(t, "0123456", shortSHA("0123456"))
	assert.Equal(t, "", shortSHA(""))
}

func TestFirstNonEmpty(t *testing.T) {
	assert.Equal(t, "a", firstNonEmpty("a", "b"))
	assert.Equal(t, "b", firstNonEmpty("", "b"))
	assert.Equal(t, "c", firstNonEmpty("", "", "c"))
	assert.Equal(t, "", firstNonEmpty("", ""))
	assert.Equal(t, "", firstNonEmpty())
}

func TestBuildDataFromBuildInfo(t *testing.T) {
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "GOOS", Value: "linux"}, // unrelated, must be ignored
		},
	}
	bd := buildDataFromBuildInfo(bi)
	assert.Equal(t, "v1.2.3", bd.mainVersion)
	assert.Equal(t, "0123456789abcdef", bd.revision)
	assert.True(t, bd.modified)
}

func TestBuildDataFromBuildInfo_NotModified(t *testing.T) {
	bi := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.modified", Value: "false"},
		},
	}
	bd := buildDataFromBuildInfo(bi)
	assert.False(t, bd.modified)
	assert.Empty(t, bd.revision)
	assert.Empty(t, bd.mainVersion)
}

func TestGet(t *testing.T) {
	// Get() must wire real build info; under `go test` it should yield a
	// non-empty version with populated runtime fields without panicking.
	info := Get()
	assert.NotEmpty(t, info.Version)
	assert.NotEmpty(t, info.Commit)
	assert.NotEmpty(t, info.Date)
	assert.Equal(t, runtime.Version(), info.GoVersion)
	assert.Equal(t, runtime.GOOS+"/"+runtime.GOARCH, info.OSArch)
}

func TestShort(t *testing.T) {
	s := Short()
	require.NotEmpty(t, s)
	assert.Equal(t, Get().Version, s)
	assert.NotContains(t, s, "\n", "Short() must be single-line")
}

func TestFull(t *testing.T) {
	info := Get()
	full := Full()

	lines := strings.Split(full, "\n")
	require.Len(t, lines, 5, "Full() must have 5 lines (Version/Commit/Date/Go/OS-Arch)")

	assert.Equal(t, fmt.Sprintf("Version:    %s", info.Version), lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "Commit:"))
	assert.True(t, strings.HasPrefix(lines[2], "Date:"))
	assert.True(t, strings.HasPrefix(lines[3], "Go version:"))
	assert.True(t, strings.HasPrefix(lines[4], "OS/Arch:"))

	assert.Contains(t, full, info.Version)
	assert.Contains(t, full, info.Commit)
	assert.Contains(t, full, info.GoVersion)
	assert.Contains(t, full, info.OSArch)
	// No trailing newline on the final line.
	assert.False(t, strings.HasSuffix(full, "\n"))
}
