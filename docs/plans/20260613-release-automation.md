# Release Automation (GoReleaser + `go install` + version stamping)

## Overview

Introduce a repeatable release process for HubFuse so that:

- Releases are **fixed** as semver git tags (`vX.Y.Z`) and published as GitHub Releases with prebuilt cross-platform binaries.
- Users can **install and update** via `go install github.com/ykhdr/hubfuse/cmd/<bin>@latest` (already viable — module path matches the repo).
- Both binaries report an accurate version (`--version` flag + `version` subcommand) regardless of how they were built (GoReleaser release, `go install @vX.Y.Z`, or local `go build`).

This solves the current gaps: there are **no git tags**, **no version stamping in code**, and **no release pipeline**. The agent (`hubfuse`) depends on FUSE/SSHFS so it targets Linux+macOS only; the hub (`hubfuse-hub`) is pure gRPC+SQLite (`modernc.org/sqlite`, pure Go → `CGO_ENABLED=0` works).

## Context (from discovery)

- **Module:** `github.com/ykhdr/hubfuse`, Go 1.25.0, remote `git@github.com:ykhdr/hubfuse`.
- **Binaries:** `cmd/hubfuse/main.go` (`rootCmd()` at ~:32, `Use: "hubfuse"`) and `cmd/hubfuse-hub/main.go` (`rootCmd()` at :25, `Use: "hubfuse-hub"`, subcommands registered at :39 via `cmd.AddCommand(startCmd(), stopCmd(), statusCmd(), issueJoinCmd())`).
- **CLI framework:** Cobra with factory functions returning `*cobra.Command`.
- **No version stamping** anywhere in code (grep clean). **No git tags** (`git tag -l` empty).
- **Existing CI:** `.github/workflows/ci.yml` (tests) and `claude.yml`. Release workflow will be a separate file triggered only on tags.
- **No LICENSE file** in repo root → GoReleaser archive must NOT hardcode a `LICENSE` path (would fail); rely on default glob-based file inclusion.
- **`goreleaser` binary not installed locally** → `goreleaser check` / `release-snapshot` are optional local steps; CI uses `goreleaser/goreleaser-action@v7`.
- **Plans:** active plans in `docs/plans/`, completed in `docs/plans/completed/`, naming `yyyymmdd-<task>.md`.

## Development Approach

- **Testing approach:** Regular (code first, then tests) — matches repo convention.
- Complete each task fully before the next; run tests after each change.
- **Tests are required** for the `internal/version` resolution logic and formatting (the only new logic with branches). Config/YAML/workflow files are validated by tooling, not unit tests.
- Do not add `main_test.go` to `cmd/*` (project rule) — version-package behavior is tested in `internal/version`, CLI wiring is exercised by existing integration/CLI tests if needed.
- `errcheck` is strict — handle all returned errors.
- Maintain backward compatibility: existing commands, `make build`, `make install`, and current CI must keep working.

## Testing Strategy

- **Unit tests:** `internal/version/version_test.go` covers `Short()`/`Full()` output shape and the precedence logic that is testable without a real build (ldflags-set path, fallback path; the `ReadBuildInfo`-dependent branches are covered as far as the test binary's build info allows, with table-driven cases on a pure helper function — see Task 1 design note).
- **Build verification:** `make vet`, `make build`, `go test ./internal/version/...`.
- **Release config verification (optional, local):** `make release-check` (`goreleaser check`) and `make release-snapshot` (`goreleaser release --snapshot --clean`) — only if `goreleaser` is installed (`go install github.com/goreleaser/goreleaser/v2@latest`). CI is the source of truth.
- No e2e/UI tests in this project.

## Progress Tracking

- Mark completed items `[x]` immediately.
- New tasks: `➕` prefix. Blockers: `⚠️` prefix.
- Keep this file in sync with actual work.

## Solution Overview

A single shared `internal/version` package holds the version state and resolution logic; both `main` packages consume it. GoReleaser injects release metadata via `ldflags` **into the `internal/version` package variables** (not `main.*`), giving one source of truth. A tag-triggered GitHub Actions workflow runs GoReleaser to cross-compile both binaries and publish a GitHub Release with archives, checksums, and auto-generated (github-native) changelog. README documents the `go install` / update path.

**Version resolution precedence** (in `internal/version`):
1. ldflags `version != ""` → GoReleaser release build → use it.
2. else `debug.ReadBuildInfo()`: if `Main.Version` is non-empty and not `"(devel)"` → `go install ...@vX.Y.Z` → use it.
3. else read `BuildInfo.Settings` `vcs.revision` (+ `vcs.modified`) → local `go build` → `dev-<short-sha>` (append `-dirty` if modified).
4. else → `dev`.

**Key design decisions (brainstormed):**
- **ldflags → `internal/version` vars, not `main.*`** — single source of truth shared by both binaries (rejected: per-binary `main.version` vars → duplication).
- **Per-build archives** (separate `hubfuse` / `hubfuse-hub` downloads) — rejected the single shared archive that bundles both binaries under one ambiguous name.
- **`{{.Date}}`** for build date — rejected `{{.CommitDate}}` for now (reproducible-build hashes not a current requirement).
- **Archives-only, manual semver tags** — rejected Homebrew/Docker/deb and automated tagging (release-please/svu) as YAGNI for the first iteration.

## Technical Details

### `internal/version` package shape

```go
package version

// set via -ldflags -X by GoReleaser; empty for go install / local builds
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

func Get() Info            // pure-ish resolver (reads ldflags vars + debug.ReadBuildInfo)
func Short() string        // "v0.2.0" — for cmd.Version / --version
func Full() string         // multi-line block — for `version` subcommand
```

**Design note (testability):** factor the precedence logic into a pure helper that takes the ldflags values and a `*debug.BuildInfo` (or a small interface/struct of the fields it needs) so `version_test.go` can table-test all four branches deterministically. `Get()` wires the real `debug.ReadBuildInfo()` into that helper.

### GoReleaser ldflags (per build)

```
-s -w
-X github.com/ykhdr/hubfuse/internal/version.version={{.Version}}
-X github.com/ykhdr/hubfuse/internal/version.commit={{.Commit}}
-X github.com/ykhdr/hubfuse/internal/version.date={{.Date}}
```

`{{.Date}}` is a valid built-in GoReleaser variable (current UTC build time, RFC3339) — it is also the GoReleaser default. Alternative: `{{.CommitDate}}` (the commit's date) for reproducible builds where the same tag always yields the same binary. We use `{{.Date}}` deliberately (the field is a build date); switch to `.CommitDate` only if reproducible-build hashes become a requirement.

### `.goreleaser.yaml` (schema v2) outline

- `version: 2`
- `before.hooks: [go mod tidy]`
- `builds:` two entries:
  - `id: hubfuse`, `main: ./cmd/hubfuse`, `binary: hubfuse`
  - `id: hubfuse-hub`, `main: ./cmd/hubfuse-hub`, `binary: hubfuse-hub`
  - both: `env: [CGO_ENABLED=0]`, `goos: [linux, darwin]`, `goarch: [amd64, arm64]`, `ldflags:` as above.
- `archives:` **per-build** (one archive entry per binary, so users can download just the hub or just the agent — a single shared archive would bundle both binaries into one confusingly-named file). Use the schema-v2 `ids:` key (replaces the older `builds:` key) to scope each archive to one build id:
  ```yaml
  archives:
    - id: hubfuse
      ids: [hubfuse]
      formats: [tar.gz]
      name_template: "hubfuse_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    - id: hubfuse-hub
      ids: [hubfuse-hub]
      formats: [tar.gz]
      name_template: "hubfuse-hub_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
  ```
  **Do not hardcode LICENSE** (none exists) — rely on default file globs (`README*` is auto-included; missing globs are silently skipped, not an error).
- `checksum: name_template: "checksums.txt"`.
- `changelog: use: github-native`.
- `release: prerelease: auto` (so `v0.2.0-rc1` → prerelease).
- `snapshot:` version-name template for `--snapshot`.

### `.github/workflows/release.yml` outline

- `on: push: tags: ["v*"]`
- `permissions: contents: write`
- job on `ubuntu-latest`:
  1. `actions/checkout@v4` with `fetch-depth: 0` (required for changelog).
  2. `actions/setup-go@v5` with `go-version-file: go.mod`.
  3. `goreleaser/goreleaser-action@v7` with `with: { distribution: goreleaser, version: "~> v2", args: release --clean }`, `env: GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}`. Pinning `version: "~> v2"` ensures the action matches the schema-`version: 2` config (default `latest` could drift).
- No extra secrets needed (archives-only → automatic `GITHUB_TOKEN` suffices).

## What Goes Where

- **Implementation Steps** (checkboxes): version package + tests, Cobra wiring, `.goreleaser.yaml`, release workflow, Makefile targets, README docs, verification.
- **Post-Completion** (no checkboxes): pushing the first real tag, watching the first release run, repo "public" requirement for third-party `go install`.

## Implementation Steps

### Task 1: Create `internal/version` package

**Files:**
- Create: `internal/version/version.go`
- Create: `internal/version/version_test.go`

- [x] create `internal/version/version.go` with ldflags vars (`version`, `commit`, `date`), `Info` struct, `Get()`, `Short()`, `Full()`.
- [x] implement a pure precedence helper (takes ldflags values + build-info fields) covering all 4 branches; `Get()` wires `debug.ReadBuildInfo()` into it.
- [x] `Short()` returns the single-line version; `Full()` returns the multi-line block (Version/Commit/Date/GoVersion/OS-Arch).
- [x] write table-driven tests for the precedence helper (ldflags-set, `go install` version, vcs-revision + dirty, bare fallback) and for `Short()`/`Full()` formatting.
- [x] run `go test ./internal/version/...` — must pass before next task.

### Task 2: Wire version into `hubfuse` CLI

**Files:**
- Modify: `cmd/hubfuse/main.go`

- [x] in `rootCmd()`: set `cmd.Version = version.Short()` (enables `--version`).
- [x] add a `versionCmd()` factory (`Use: "version"`) whose `Run` prints `version.Full()`.
- [x] register `versionCmd()` on the root command (alongside existing subcommands).
- [x] `make build` + manual check: `go run ./cmd/hubfuse version` and `--version` print sane output.
- [x] run `go vet ./...` and `go test ./...` for affected packages — must pass before next task.

### Task 3: Wire version into `hubfuse-hub` CLI

**Files:**
- Modify: `cmd/hubfuse-hub/main.go`

- [x] in `rootCmd()` (:25): set `cmd.Version = version.Short()`.
- [x] add a `versionCmd()` factory printing `version.Full()`.
- [x] add `versionCmd()` to the existing `cmd.AddCommand(...)` at :39.
- [x] `make build` + manual check: `go run ./cmd/hubfuse-hub version` and `--version`.
- [x] run `go vet ./...` + build — must pass before next task.

### Task 4: Add `.goreleaser.yaml`

**Files:**
- Create: `.goreleaser.yaml`

- [ ] write schema-v2 config per Technical Details (two builds, ldflags into `internal/version`, `CGO_ENABLED=0`, linux/darwin × amd64/arm64).
- [ ] **per-build** archives (two `archives` entries scoped via `ids:` — one for `hubfuse`, one for `hubfuse-hub` — with distinct `name_template` each) so the two binaries ship as separate downloads; no hardcoded LICENSE.
- [ ] checksums.txt; `changelog.use: github-native`; `release.prerelease: auto`; `before.hooks: [go mod tidy]`.
- [ ] if `goreleaser` available: `goreleaser check` passes; else note as deferred to CI. (verification — no unit test applies)

### Task 5: Add release GitHub Actions workflow

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] tag-triggered (`v*`) workflow per outline: checkout (fetch-depth 0), setup-go (go-version-file), goreleaser-action@v7 with `distribution: goreleaser` + `version: "~> v2"` + `args: release --clean`, `GITHUB_TOKEN`.
- [ ] `permissions: contents: write`; confirm it does not collide with `ci.yml`/`claude.yml`.
- [ ] sanity-check YAML (e.g. `yamllint`/`actionlint` if available, otherwise visual). (config file — no unit test applies)

### Task 6: Add Makefile release helpers

**Files:**
- Modify: `Makefile`

- [ ] add `release-snapshot` target → `goreleaser release --snapshot --clean`.
- [ ] add `release-check` target → `goreleaser check`.
- [ ] add both to `.PHONY`; leave `install`/`build` unchanged.
- [ ] if `goreleaser` installed, run `make release-check`; otherwise document install hint (`go install github.com/goreleaser/goreleaser/v2@latest`). (tooling target — no unit test applies)

### Task 7: Update README

**Files:**
- Modify: `README.md`

- [ ] add "Install via `go install`" with both binaries (`...@latest`), near the existing `make install` section (~:51).
- [ ] add "Updating" subsection (re-run the same `go install ...@latest`).
- [ ] mention prebuilt binaries from GitHub Releases.
- [ ] document `hubfuse version` / `hubfuse-hub version` and `--version`.
- [ ] (docs — no unit test applies)

### Task 8: Verify acceptance criteria

- [ ] `make vet` passes.
- [ ] `make build` passes (both binaries compile).
- [ ] `go test ./internal/version/...` passes; `make test` (or at least `make test-unit`) still green.
- [ ] `go run ./cmd/hubfuse version` and `./cmd/hubfuse-hub version` show resolved version (expected `dev-<sha>` locally).
- [ ] `goreleaser check` passes if installed (optional).
- [ ] verify Overview requirements are met (tags-as-releases pipeline, `go install` documented, version reported in all build modes).

### Task 9: [Final] Docs & plan housekeeping

- [ ] add a note to CLAUDE.md "Key Patterns": release versioning lives in `internal/version`, and GoReleaser ldflags inject into that package (not `main.*`) — future contributors must know where to wire version strings.
- [ ] move this plan to `docs/plans/completed/`.
- [ ] commit + push (per repo workflow).

## Post-Completion

*Informational — external/manual actions, no checkboxes.*

**Repo visibility:** for third parties to `go install github.com/ykhdr/hubfuse/...@latest`, the GitHub repo must be public (so `proxy.golang.org` can serve it). If private, consumers need `GOPRIVATE=github.com/ykhdr/*` + git auth.

**First release (manual):** after merge, create the first tag and push it to trigger the pipeline:
```
git tag -a v0.1.0 -m "First release"
git push origin v0.1.0
```
Then watch the `release.yml` run and confirm the GitHub Release contains both binaries' archives + `checksums.txt`.

**Update path for users:** re-running `go install github.com/ykhdr/hubfuse/cmd/<bin>@latest` pulls the newest tag (module proxy may cache `@latest` briefly).

**Future (deferred / YAGNI):** Homebrew tap, Docker image for the hub, `.deb`/`.rpm` via nfpm, and automated tagging (release-please/svu) — intentionally out of scope for this iteration.
