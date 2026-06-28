# Add `hubfuse restart` subcommand (issue #62)

> **Status: completed** (branch `feature/issue-62-restart-command`).
> Implemented `ChildArgs` override (`spawn_unix.go`), `restartCmd` + shared
> `spawnAgentDaemon` helper (`main.go`), unit test `TestSpawn_ChildArgsOverride`,
> and docs. Auto plan-review and a high-effort auto code-review were applied
> (latent empty-slice fallback, spawn-tail duplication, and logging-parity
> caveat all addressed). `make build`/`vet`/`test-unit` pass. The full
> start→restart→stop cycle requires a reachable hub + joined agent and remains
> manual (see Post-Completion) — it is not runnable in CI without a hub.

## Overview
- Add a `restart` subcommand to the `hubfuse` agent binary that stops the running daemon (if any) and starts a fresh detached one.
- Solves the inconvenience of running `hubfuse stop && hubfuse start -d` by hand after a config change or upgrade.
- Integrates by reusing the existing daemon-control primitives in `internal/common/daemonize` (`CheckRunning`, `SignalStop`, `Spawn`) — no new control logic, just orchestration.

## Context (from discovery)
- Files/components involved:
  - `cmd/hubfuse/main.go` — Cobra command tree. `rootCmd()` (~L61) wires `start/stop/status`; `startCmd()` (~L339) spawns the detached daemon; `stopCmd()` (~L428) calls `daemonize.SignalStop`.
  - `internal/common/daemonize/spawn_unix.go` — `Spawn`/`SpawnOpts`; child argv currently derived from `stripDaemonFlag(os.Args[1:])`.
  - `internal/common/daemonize/spawn_windows.go` — Windows stub of `SpawnOpts`/`Spawn` (always errors).
  - `internal/common/daemonize/control.go` — `SignalStop` (SIGTERM→SIGKILL, blocks until the process is gone), `ReportStatus`.
  - `internal/common/daemonize/daemonize.go` — `CheckRunning` (liveness + stale-pidfile cleanup), `WritePIDFile`, `IsChild`.
  - `internal/common/paths.go` — `AgentPIDFile`, `AgentLogFile`, `AgentDataDir`.
- Related patterns found: each subcommand is a `func xCmd() *cobra.Command`; daemonized start re-execs the binary with `HUBFUSE_DAEMONIZED=1` and the child takes the `IsChild()` foreground path, writing the pidfile via `OnReady`.
- Dependencies identified: Cobra; `daemonize` package; no new third-party deps.

## Development Approach
- **testing approach**: Regular (code first, then tests) — matches the existing daemonize test style (self-reexec trampoline in `spawn_unix_test.go`).
- Complete each task fully before moving to the next; small focused changes.
- **Every task includes tests.** Unit tests for the new `ChildArgs` behavior; the `restartCmd` orchestration is thin glue over already-tested primitives, so coverage targets the one new primitive (`ChildArgs`) plus a build/vet gate.
- **All tests must pass before starting the next task.**
- Maintain backward compatibility: `ChildArgs` is additive and defaults to current behavior when nil.

## Testing Strategy
- **unit tests**: extend `internal/common/daemonize/spawn_unix_test.go` to assert that `SpawnOpts.ChildArgs` is the argv the child actually receives, and that a nil `ChildArgs` preserves the `os.Args`-derived behavior.
- **e2e tests**: none — project has no UI/e2e harness. Full restart cycle (stop+respawn) is validated manually (see Post-Completion) since it forks real processes.

## Progress Tracking
- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this plan in sync with actual work.

## Solution Overview
- **Approach (Option A from brainstorm): `ChildArgs` override on `SpawnOpts`.**
- The hazard: `Spawn` re-execs `os.Args[1:]`. If `restart` called `Spawn` unchanged, the detached child would re-run `hubfuse restart`, whose own `Spawn` errors out under the `IsChild()` guard (`spawn_unix.go:43`) — leaving **no working daemon** (broken, not a literal fork bomb, since the child inherits `HUBFUSE_DAEMONIZED=1`). The `ChildArgs` override is required for correctness either way.
- Fix: let the caller override the child argv. `restartCmd` passes `ChildArgs: []string{"start"}`, so the detached child runs `hubfuse start` with `HUBFUSE_DAEMONIZED=1`, hits the existing `IsChild()` foreground path, runs the daemon and writes the pidfile — identical to a normal `start -d` child.
- `restartCmd` orchestration (mirrors `startCmd`'s error handling): `CheckRunning(pidPath)` → on `err != nil` return it; if alive, `SignalStop(pidPath, "agent")` and **return on its error — never `Spawn` after a failed stop** (else a second daemon could start while the first is still alive); if not alive, print a "not running" notice and continue. Then `os.MkdirAll(dataDir, 0o700)` and `Spawn{LogPath, PIDFilePath, ChildArgs: ["start"]}`.
- No extra sleep/race handling: `SignalStop` returns only after `waitForExit` observes ESRCH, by which point the old process's `defer os.Remove(pidPath)` has run; `Spawn`/`WritePIDFile` then writes a fresh pidfile via atomic rename, and the child's own `CheckRunning` would clear any stale file anyway.

## Technical Details
- **`SpawnOpts.ChildArgs []string`** (new field):
  - unix (`spawn_unix.go`): `childArgs := opts.ChildArgs; if childArgs == nil { childArgs = stripDaemonFlag(os.Args[1:]) }`. Nil → unchanged behavior.
  - windows (`spawn_windows.go`): add the field for struct-literal compatibility; `Spawn` still returns the not-supported error, so the field is ignored.
- **`restartCmd()`** in `cmd/hubfuse/main.go`:
  - paths from `common.ExpandHome(common.AgentDataDir)`, `common.AgentPIDFile`, `common.AgentLogFile` (mirrors `startCmd`).
  - No flags in v1 — `restart` always backgrounds (a foreground restart is meaningless); the child inherits `start`'s flag defaults (log level `debug`, stdout→`agent.log`).
- **Registration**: add `restartCmd()` to the `cmd.AddCommand(...)` list in `rootCmd()`, right after `stopCmd()`.
- **Test trampoline**: extend the existing `ready` role in `spawn_unix_test.go` to also write `strings.Join(os.Args[1:], "\n")` to the path in a new `HUBFUSE_TEST_ARGSFILE` env var (only when set) before blocking on signals. Existing tests don't set the env, so they're unaffected; the new test sets `ChildArgs` + the env and asserts the dumped argv equals `ChildArgs`.

## What Goes Where
- **Implementation Steps** (`[ ]`): the `ChildArgs` field, the `restartCmd`, registration, and unit tests — all in-repo.
- **Post-Completion** (no checkboxes): manual end-to-end restart verification with a real daemon; README/help-output note.

## Implementation Steps

### Task 1: Add `ChildArgs` override to `SpawnOpts`

**Files:**
- Modify: `internal/common/daemonize/spawn_unix.go`
- Modify: `internal/common/daemonize/spawn_windows.go`
- Modify: `internal/common/daemonize/spawn_unix_test.go`

- [ ] add `ChildArgs []string` field to `SpawnOpts` in `spawn_unix.go` with a doc comment (nil → derive from `os.Args`)
- [ ] in `Spawn`, replace `childArgs := stripDaemonFlag(os.Args[1:])` with the nil-check that prefers `opts.ChildArgs`
- [ ] add the same `ChildArgs []string` field to the Windows `SpawnOpts` stub (documented as ignored)
- [ ] extend the `ready` trampoline role to dump `os.Args[1:]` to `HUBFUSE_TEST_ARGSFILE` when that env is set — write it **before** `WritePIDFile` so the file is guaranteed present once `Spawn` returns (avoids a read race)
- [ ] write test `TestSpawn_ChildArgsOverride`: set `ChildArgs: []string{"start", "--marker"}` + the argsfile env, run `Spawn`, assert the dumped argv equals `ChildArgs`; then SIGTERM the child and `waitForDeath`
- [ ] run tests — must pass before next task: `go test ./internal/common/daemonize/...`

### Task 2: Add `restartCmd` and register it

**Files:**
- Modify: `cmd/hubfuse/main.go`

- [ ] add `restartCmd()` returning a `*cobra.Command` (`Use: "restart"`, `Short: "Restart the agent daemon"`) that resolves `dataDir`/`pidPath`/`defaultLog`
- [ ] implement orchestration: `CheckRunning` (return on its error) → if alive `SignalStop` (return on its error — never `Spawn` after a failed stop) else print "agent is not running; starting a new daemon"; then `os.MkdirAll(dataDir, 0o700)`; then `Spawn{LogPath: defaultLog, PIDFilePath: pidPath, ChildArgs: []string{"start"}}`
- [ ] register `restartCmd()` in `rootCmd()`'s `AddCommand` list directly after `stopCmd()`
- [ ] run `make build` and `make vet` — must pass before next task

### Task 3: Verify acceptance criteria
- [ ] `restart` with a running daemon stops it then starts a fresh one (verified manually — see Post-Completion)
- [ ] `restart` with no daemon running starts one without erroring (no-op stop path)
- [ ] confirm no recursion: the detached child runs `start`, not `restart`
- [ ] run full unit suite: `make test-unit`
- [ ] `make vet` clean

### Task 4: Update documentation
- [ ] update `README.md` command list if it enumerates agent subcommands (add `restart`)
- [ ] update `CLAUDE.md` only if a new pattern was introduced (none expected — note `ChildArgs` if useful)
- [ ] move this plan to `docs/plans/completed/`
- [ ] commit and push all changes (per CLAUDE.md workflow), then open the PR

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**Manual verification:**
- On a Unix host: `hubfuse start -d`, then `hubfuse restart`, then `hubfuse status` — confirm a new PID, `agent.log` shows a clean shutdown + fresh startup, and only one daemon is alive.
- `hubfuse restart` with nothing running — confirm it starts a daemon and prints the "not running" notice rather than an error.
- Windows: `restart` surfaces the existing "not supported on windows" error from `Spawn` (no regression).
