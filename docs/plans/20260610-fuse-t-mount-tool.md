# Add FUSE-T as an Alternative Mount Tool

## Overview

Add support for selecting **FUSE-T** as the mount backend on macOS, alongside the
existing default `sshfs` (macFUSE) path. The goal is a smoother macOS onboarding
experience: macFUSE requires a kernel extension (System Settings approval, a
reboot, and reduced-security mode on Apple Silicon), whereas FUSE-T is kext-free
(it uses a local NFS server) and ships a drop-in `sshfs` binary.

**Key insight:** this is *not* a new mount mechanism. The current `sshfs` command
already works with FUSE-T's `sshfs` unchanged, because every flag we pass
(`-p`, `-o IdentityFile`, `-o StrictHostKeyChecking`, `-o UserKnownHostsFile`)
is an **SSH** option that `sshfs` forwards to `ssh` — not a FUSE option. So the
work is mostly config plumbing, validation, a startup pre-flight check, and docs.

The selected approach is a **lightweight backend profile** (a small lookup table),
not a full backend interface — YAGNI. The tool is selected via a single
device-global config option; per-mount override is intentionally deferred.

## Context (from discovery)

- **Files/components involved:**
  - `internal/agent/config/config.go` — KDL config parser; `AgentConfig` holds `SSHPort`.
  - `internal/agent/mounter.go` — `Mounter` shells out to `sshfs` (hardcoded literal, lines 90–97); `unmountPath` is platform-specific (`umount` on darwin, `fusermount -u` else, lines 187–194).
  - `internal/agent/daemon.go` — constructs the mounter (~line 83) via `NewMounter(keyPath, knownDevicesDir, knownHostsDir, logger)`.
  - `internal/agent/config/config_test.go`, `internal/agent/mounter_test.go` — existing tests.
  - `README.md`, `CLAUDE.md` — docs.
- **Related patterns found:**
  - Config format is **KDL** (`github.com/sblinch/kdl-go`); node names are kebab-case (`ssh-port`). `permissions` is handled as a plain validated string, not a Go enum.
  - Mounter is a concrete struct with DI hooks for tests: `SetExecCommandForTests` (line 270) captures the exec args; `SetUnmountForTests` (line 277).
  - `DefaultConfig()` applies defaults before parsing so omitted fields keep defaults.
- **Dependencies identified:** `os/exec` (`LookPath`, `CommandContext`), `runtime` (already imported in `mounter.go`). No new third-party deps.

## Development Approach

- **Testing approach**: Regular (code first, then tests) — each task pairs the
  implementation with its tests before moving on, matching Go conventions in this repo.
- Complete each task fully before moving to the next; small, focused changes.
- **Every task includes new/updated tests** (success + error/edge cases).
- **All tests must pass before starting the next task.**
- Update this plan file if scope changes during implementation.
- Maintain backward compatibility: omitting `mount-tool` keeps today's behavior exactly.

### Repo conventions to honor

- `errcheck` is strict — handle/return every error (including in tests; assign with `_ =` only where deliberate).
- No `cmd/*/main_test.go` — test lower layers only (config, mounter).
- No real-mount / integration test that shells out to a real `sshfs` — keep the existing exec-hook mock approach.
- Work stays on the feature worktree/branch (`thirsty-margulis-9c879e` / `claude/thirsty-margulis-9c879e`), never `master`.
- Commit + push after completing the work.

## Testing Strategy

- **Unit tests**: required for every task.
  - Config parse/default/validation/round-trip.
  - Pure helpers `resolveBackend`, `buildMountArgs`, `validateMountTool`.
  - Pre-flight binary check via an injectable `LookPath` stub or a guaranteed-absent binary name.
- **E2E tests**: none — this project has no UI e2e suite, and we deliberately do
  not shell out to a real `sshfs` in CI.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document blockers with ⚠️ prefix.
- Keep the plan in sync with actual work.

## Solution Overview

1. A new device-global config key `mount-tool` in the `agent { }` block, validated
   to `""` / `"sshfs"` / `"fuse-t"` (empty → default `sshfs`).
2. A `mountBackend` lookup table in `mounter.go` as the single source of truth for
   what each tool needs (`binary` + `extraOpts`). Both profiles use the `sshfs`
   binary today (macFUSE-sshfs and fuse-t-sshfs install to the same path and
   collide — the user has exactly one); `extraOpts` is empty for both for now.
3. Arg-slice refactor of `Mount()` so backend `extraOpts` can be injected; the
   `unmount` path is untouched.
4. Three validation layers: value validation at config load; darwin-only platform
   gating at daemon startup; binary pre-flight (`exec.LookPath`) at startup when
   mounts are configured — **warn and continue** on miss so sharing still works.
5. Pure helpers (`resolveBackend`, `buildMountArgs`, `validateMountTool`) extracted
   for clean, platform-independent unit tests.
6. Docs in README, config reference/example, and CLAUDE.md.

## Technical Details

### Config (`AgentConfig`)

```go
type AgentConfig struct {
    SSHPort   int
    MountTool string // "sshfs" (default) | "fuse-t"
}
```

KDL:

```kdl
agent {
    ssh-port 2222
    mount-tool "fuse-t"   // "sshfs" (default) | "fuse-t"
}
```

### Backend profile (`mounter.go`)

```go
type mountBackend struct {
    name      string   // "sshfs" | "fuse-t"
    binary    string   // executable to run
    extraOpts []string // backend-specific -o options appended to the command
}

var mountBackends = map[string]mountBackend{
    "sshfs":  {name: "sshfs",  binary: "sshfs", extraOpts: nil},
    "fuse-t": {name: "fuse-t", binary: "sshfs", extraOpts: nil}, // fuse-t ships a drop-in sshfs
}
```

### Command construction (refactored from lines 90–97)

```go
args := buildMountArgs(m.backend, sshPort, m.keyPath, knownHostsPath, deviceIP, mc.Share, mc.To)
cmd := m.execCommand(ctx, m.backend.binary, args...)
```

where `buildMountArgs` builds the base args, appends `extraOpts` as ordered
`-o <opt>` pairs, then appends the `hubfuse@<ip>:<share>` source and `<to>` target
operands last.

### Validation / pre-flight

- `validateMountTool(tool, goos string) error` — rejects unknown values on any OS;
  rejects `"fuse-t"` when `goos != "darwin"` with
  `mount-tool "fuse-t" is only supported on macOS`.
- Startup binary pre-flight (only when `len(cfg.Mounts) > 0`): `exec.LookPath(backend.binary)`;
  on miss, **log a warning and continue** with an actionable message:
  `fuse-t selected but "sshfs" not found on PATH — install with: brew install --cask fuse-t fuse-t-sshfs`.

### Non-goals (YAGNI)

- Do **not** detect which FUSE impl backs the `sshfs` binary (both are `sshfs`;
  sniffing the install layout is brittle).
- Do **not** add per-mount tool override (deferred; the global option can be
  overridden per-mount later without breaking this).
- Do **not** duplicate per-mount error handling — `cmd.Start()` (line 99) still
  returns hard failures.

## What Goes Where

- **Implementation Steps** (`[ ]`): config field + parse/save + validation, backend
  table + helpers, mounter wiring, daemon wiring + pre-flight, tests, docs.
- **Post-Completion** (no checkboxes): manual verification on a real Mac with
  FUSE-T installed.

## Implementation Steps

### Task 1: Add `mount-tool` config field, parsing, default, save, and validation

**Files:**
- Modify: `internal/agent/config/config.go`
- Modify: `internal/agent/config/config_test.go`

- [x] add `MountTool string` field to `AgentConfig` (after the `SSHPort` field, ~line 38) with a doc comment listing allowed values and the default
- [x] set `MountTool: "sshfs"` in `DefaultConfig()` (~line 58)
- [x] add `case "mount-tool":` to `parseAgentConfig` (~line 133) reading `firstArgString(child)` into `ac.MountTool`
- [x] in `Load` (after parsing, ~line 95), validate `cfg.Agent.MountTool`: accept `""`/`"sshfs"`/`"fuse-t"`; normalise `""` → `"sshfs"`; otherwise return a clear error listing allowed values (do **not** apply OS gating here — that is platform-specific and lives in the daemon layer)
- [x] write the `mount-tool` line inside the agent block in `Save()` (~line 269): `fmt.Fprintf(&sb, "    mount-tool %q\n", cfg.Agent.MountTool)`
- [x] write tests: `mount-tool "fuse-t"` parses to `MountTool == "fuse-t"`; omitted → default `"sshfs"`; invalid value → `Load` returns error; Save/Load round-trip preserves the value
- [x] run tests: `go test ./internal/agent/config/...` — must pass before next task

### Task 2: Add backend profile table and pure helpers in the mounter

**Files:**
- Modify: `internal/agent/mounter.go`
- Modify: `internal/agent/mounter_test.go`

- [x] add `mountBackend` struct and the `mountBackends` lookup map (`"sshfs"`, `"fuse-t"`, both `binary: "sshfs"`, `extraOpts: nil`) with comments explaining the same-binary/collision rationale
- [x] add `resolveBackend(tool string) mountBackend` — map lookup, empty → `"sshfs"`, unknown → fall back to `"sshfs"` profile (config `Load` already rejects unknowns; this is a defensive default)
- [x] add `buildMountArgs(b mountBackend, sshPort int, keyPath, knownHosts, deviceIP, share, to string) []string` — base args (`-p`, `-o IdentityFile=`, `-o StrictHostKeyChecking=yes`, `-o UserKnownHostsFile=`), then `extraOpts` as ordered `-o <opt>` pairs, then `hubfuse@<ip>:<share>` and `<to>` operands last (use `strconv.Itoa` for the port)
- [x] add `validateMountTool(tool, goos string) error` — unknown value → error on any OS; `"fuse-t"` && `goos != "darwin"` → `mount-tool "fuse-t" is only supported on macOS`; otherwise nil
- [x] write tests for `resolveBackend` (`"sshfs"`, `"fuse-t"`, `""`→sshfs, unknown→sshfs)
- [x] write tests for `buildMountArgs` (base args correct; non-empty `extraOpts` injected as ordered `-o` pairs *before* the `user@host:share`/`to` operands)
- [x] write tests for `validateMountTool` (`("fuse-t","linux")`→error, `("fuse-t","darwin")`→ok, `("sshfs","linux")`→ok, bad value→error on any OS)
- [x] run tests: `go test ./internal/agent/...` — must pass before next task

### Task 3: Wire the backend into the Mounter and refactor `Mount()`

**Files:**
- Modify: `internal/agent/mounter.go`
- Modify: `internal/agent/mounter_test.go`
- Modify: `internal/agent/daemon.go`

- [x] add a `backend mountBackend` field to the `Mounter` struct
- [x] change `NewMounter` to accept a `mountTool string` parameter and set `backend: resolveBackend(mountTool)`
- [x] refactor `Mount()` (lines 90–97) to call `buildMountArgs(...)` and `m.execCommand(ctx, m.backend.binary, args...)` — behavior for the default `sshfs` tool must be byte-identical to today
- [x] update the `NewMounter(...)` call in `daemon.go` (~line 83) to pass `cfg.Agent.MountTool`
- [x] update the two other `NewMounter` callers for the new signature: `internal/agent/mounter_test.go:32` and `tests/integration/prune_test.go:90` (the latter lives outside `internal/agent/...`, so `go test ./internal/agent/...` will NOT catch it — `go build ./...` and the integration suite will)
- [x] extend the existing `Mount` test to assert the selected `binary` and full arg list flow through `SetExecCommandForTests` (line 270); add a `"fuse-t"` case confirming the captured binary
- [x] run tests: `go build ./...`, `go test ./internal/agent/...`, and `go test ./tests/integration/...` — must pass before next task

### Task 4: Daemon startup platform gating + binary pre-flight

**Files:**
- Modify: `internal/agent/daemon.go`
- Modify: `internal/agent/daemon_test.go` (already exists — package `agent`, testify, `buildTestDaemon` helper)

- [x] add `"runtime"` to the `daemon.go` import block (it is currently imported only in `mounter.go`, not `daemon.go`)
- [x] at daemon startup (before/around mounter construction, ~line 83), call `validateMountTool(cfg.Agent.MountTool, runtime.GOOS)` and return the error if non-nil (fail fast on a misconfigured OS)
- [x] add a binary pre-flight: only when `len(cfg.Mounts) > 0`, run `exec.LookPath(resolveBackend(cfg.Agent.MountTool).binary)`; on error, **log a warning and continue** with the actionable install message (do not abort — sharing must still work)
- [x] make the pre-flight unit-testable: extract a small **pure** helper (e.g. `preflightMountBinary(backend mountBackend, hasMounts bool, lookPath func(string) (string, error), logger *slog.Logger)`) so the `LookPath` dependency can be stubbed and no daemon instance is needed
- [x] write tests (in `daemon_test.go`): pre-flight with stubbed `lookPath` returning error → warning emitted, no error returned; pre-flight with `hasMounts == false` → `lookPath` not called; (platform gating already covered by `validateMountTool` tests in Task 2)
- [x] run tests: `go test ./internal/agent/...` — must pass before next task

### Task 5: Documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: config example / reference (whichever file documents the KDL config; e.g. README config section or a sample `.kdl`)

- [ ] README macOS install section: recommend FUSE-T as the kext-free path (`brew install --cask fuse-t fuse-t-sshfs`), contrast with macFUSE (kext approval + reboot, reduced-security on Apple Silicon); note FUSE-T is macOS-only and Linux uses distro `sshfs` + `fusermount`
- [ ] document `agent { mount-tool "..." }`: values `"sshfs"` (default) / `"fuse-t"`, device-global, `"fuse-t"` requires macOS + `fuse-t-sshfs`
- [ ] CLAUDE.md: one line under the mounter description noting the mount tool is selectable via `mount-tool`, pointing to the `mountBackends` table
- [ ] (docs-only task — no new code tests; verify examples are valid KDL by loading one in an existing config test if convenient)

### Task 6: Verify acceptance criteria

- [ ] omitting `mount-tool` produces byte-identical `sshfs` invocation as before (backward compatible)
- [ ] `mount-tool "fuse-t"` on darwin builds the correct command; on Linux fails fast with the macOS-only error
- [ ] missing binary with mounts configured → warning logged, daemon continues, sharing works
- [ ] run full suite: `make test` (runs `test-unit`, `test-integration`, `test-cli`, `test-scenarios` — see Makefile)
- [ ] `make vet` clean; `errcheck` clean

### Task 7: Finalize

- [ ] move this plan to `docs/plans/completed/`
- [ ] commit + push on the feature branch

## Post-Completion

*Manual / external verification — informational only, no checkboxes.*

**Manual verification on a real Mac:**
- Install FUSE-T: `brew install --cask fuse-t fuse-t-sshfs`.
- Set `mount-tool "fuse-t"` in the agent config and confirm a remote share mounts
  and is browsable without any macFUSE kernel extension installed.
- Confirm unmount works via the existing `umount` path.
- Confirm the warn-and-continue path: with `mount-tool "fuse-t"` but `fuse-t-sshfs`
  uninstalled, the daemon starts, logs the install hint, and per-mount attempts
  fail with a clear error while sharing keeps working.
