# Daemon mode for `hubfuse-agent` and `hubfuse-hub`

**Issue:** [#2](https://github.com/ykhdr/hubfuse/issues/2) — `hubfuse-agent start` blocks the
terminal, no daemon mode.

## Problem

Both `hubfuse-agent start` and `hubfuse-hub start` run in the foreground and hold the
terminal until SIGINT. To run any follow-up command (`pair`, `devices`, `mount`, hub
inspection, etc.) the user must open a second terminal or fall back to
`nohup hubfuse-agent start &`. The issue specifically calls this out for the agent;
the hub has the same limitation.

A related readiness problem surfaces once we add daemon mode: today the agent's
PID file is written in the `cmd` layer *before* `daemon.Run` is called, so the
PID file's appearance does not mean the agent has actually connected to the hub.
The hub writes its PID file inside `hub.Start` after `net.Listen`, which is
stricter. Daemon mode needs a single, well-defined readiness point — PID file
present = service ready — so the design below moves PID-file writes behind an
`OnReady` hook in both services.

## Goals

1. Ship a built-in `--daemon` / `-d` flag on both `start` commands that detaches
   the process from the controlling terminal and returns immediately.
2. Make daemon mode opt-in. Foreground stays the default so existing usage and
   any future systemd/launchd unit files keep working unchanged.
3. Write all daemon logs to a predictable file so the user can `tail` them.
4. Prevent a second daemon of the same kind from starting while one is already
   running; clean up stale PID files automatically.
5. Share the daemonization code between the two binaries.

## Non-goals

- Log rotation. Users who need it can configure `logrotate` / `launchd` /
  `systemd` around the log file. Keeps scope small (YAGNI).
- Windows support. The project already depends on SSHFS and POSIX process
  semantics; on Windows the new flag will fail with a clear
  "not supported" message.
- Replacing the existing foreground mode with systemd-style supervision
  (`sd_notify`, watchdog, socket activation). Out of scope for this issue.

## Design

### New package: `internal/common/daemonize`

Single home for the fork-and-exec logic, usable from both `cmd/hubfuse-agent`
and `cmd/hubfuse-hub`. Built with `//go:build unix`; a
`daemonize_windows.go` stub returns a clear error from `Spawn`.

Exported API:

```go
// IsChild reports whether the current process is the detached child
// spawned by Spawn. Detected via env HUBFUSE_DAEMONIZED=1.
func IsChild() bool

// SpawnOpts configures Spawn.
type SpawnOpts struct {
    LogPath      string        // stdout+stderr of the child go here (O_APPEND)
    PIDFilePath  string        // appearance of this file = child is ready
    ReadyTimeout time.Duration // default 5s; caller can override
}

// Spawn re-execs the current binary with HUBFUSE_DAEMONIZED=1 and
// SysProcAttr{Setsid: true}, redirects stdio, and waits until the child
// either (a) writes PIDFilePath while still alive, or (b) exits, or
// (c) times out. On (a) returns nil after printing
// "started (pid N, logs: <path>)". On (b) returns an error containing
// the last lines of the log. On (c) kills the child and returns an
// error. Must only be called when IsChild() is false.
func Spawn(opts SpawnOpts) error

// CheckRunning returns (pid, true, nil) if pidFilePath points at a live
// process. If the file exists but the process is gone it removes the
// stale file and returns (0, false, nil). I/O problems surface as err.
func CheckRunning(pidFilePath string) (int, bool, error)

// WritePIDFile atomically writes os.Getpid() to pidFilePath
// (via tmp file + rename).
func WritePIDFile(pidFilePath string) error
```

**Argv rewriting.** Before re-exec, Spawn strips every form of the daemon
flag (`--daemon`, `-d`, `--daemon=true`, `--daemon=false`) from `os.Args[1:]`
so that even if someone removed the `IsChild()` guard a second fork could not
recurse.

**Readiness signal.** The parent polls for the PID file every 50 ms up to
`ReadyTimeout`. Each service writes its PID file from an `OnReady` callback
that fires after its listeners are up and its initial handshake has
completed (see "Readiness hook" below), so the parent's `"started"` line
implies the service is actually accepting work. During the wait the parent
also watches `cmd.Process.Wait`; if the child exits before writing the
file, Spawn tails the last ~20 lines of the log and returns them in the
error.

### Readiness hook

Both `agent.Daemon` and `hub.Hub` gain an exported `OnReady func()` field.
Each is called exactly once, immediately after the service is in a state
where follow-up client commands will succeed:

- `hub.Hub.OnReady` — fires right after `net.Listen` returns in
  `hub.Start`. The existing inline `os.WriteFile(pidFile, ...)` call at
  `internal/hub/hub.go:111–114` is removed; the `cmd` layer supplies a
  callback that performs the PID-file write instead.
- `agent.Daemon.OnReady` — fires after step 4 of `Daemon.Run` (Register
  with hub succeeded). Before that point the agent cannot service any
  RPC-driven commands, so reporting "started" earlier would be a lie.

The `cmd` layer provides the callback:
`func() { _ = daemonize.WritePIDFile(pidPath) }`.
Logging the error inside the callback is left to `WritePIDFile` itself.

### `hubfuse-agent start`

Add flags:

- `--daemon` / `-d` (bool, default false) — detach.
- `--log-output <stderr|path>` (string, default `stderr` in foreground,
  `~/.hubfuse/agent.log` in daemon). Matches the existing hub flag.
- `--log-level <debug|info|warn|error>` (string, default `info`). Matches
  the existing hub flag. The agent currently hard-codes `Info`; adding this
  flag in the same place we already touch costs three lines and avoids a
  follow-up visit to the same file. If treated as out-of-scope it can be
  dropped from this PR without affecting the rest.

`RunE` becomes:

```
1. Resolve dataDir, pidPath (~/.hubfuse/hubfuse-agent.pid),
   logPath (from --log-output, defaulted by daemon flag).
2. pid, alive, _ := CheckRunning(pidPath)
   if alive: return fmt.Errorf("agent already running (pid %d)", pid)
3. if daemon && !IsChild():
       return Spawn(SpawnOpts{logPath, pidPath, 5s})
4. Initialize logger against logPath (or stderr).
5. Create daemon (agent.NewDaemon), set
   daemon.OnReady = func() { _ = daemonize.WritePIDFile(pidPath) }.
6. Install SIGTERM/SIGINT handler, daemon.Run(ctx),
   defer os.Remove(pidPath).
   (The existing unconditional PID-file write before Run is removed.)
```

The PID-file check in step 2 applies in both foreground and daemon mode for
consistency — a dangling `.pid` after an earlier crash no longer causes
silent overwrite.

### `hubfuse-hub start`

Same flow. Specifics:

- New flag: `--daemon` / `-d`. Existing `--listen`, `--data-dir`,
  `--log-level`, `--log-output` unchanged.
- `dataDir` default is `~/.hubfuse-hub`; `pidPath` is
  `<dataDir>/hubfuse-hub.pid` (matches what `stop`/`status` already read
  and what `hub.Start` currently writes inline).
- The PID-file write that today lives inside `hub.Start` moves out to the
  `cmd` layer, supplied via `hub.OnReady`. Path is unchanged.
- `logPath` resolution: if user passed `--log-output`, honour it; otherwise
  `stderr` in foreground, `<dataDir>/hub.log` in daemon.

### Shared helper

```go
// resolveLogOutput picks the effective log destination.
// userFlag is what the user passed (possibly the CLI default);
// defaultPath is what we want in daemon mode when userFlag is "stderr".
func resolveLogOutput(userFlag string, daemon bool, defaultPath string) string
```

Lives in `internal/common/daemonize` alongside the rest. Small, pure,
trivially unit-testable.

### Error handling and edges

- `logPath` is resolved to an absolute path in the parent before Spawn, so
  the child's cwd does not matter.
- `Stdin` of the child is bound to `/dev/null` — prevents anything that
  calls `bufio.Reader.ReadString(os.Stdin)` from blocking silently.
- If the log file cannot be opened (permissions, read-only FS) Spawn fails
  in the parent with a clear error; nothing is forked.
- Timeout path kills the child with `cmd.Process.Kill()` before returning,
  so a hung service does not survive as an orphan.
- Windows build returns `errors.New("--daemon is not supported on windows")`
  from a stub `Spawn`. `IsChild` is always false there.

## Testing

### Unit tests in `internal/common/daemonize`

- `TestIsChild_EnvSet` / `TestIsChild_EnvUnset` — trivial, `t.Setenv`.
- `TestCheckRunning_NoPIDFile` — returns `(0, false, nil)`.
- `TestCheckRunning_StalePID` — write a PID that cannot exist (e.g. one we
  spawn and `Wait` on), assert `(0, false, nil)` and file removed.
- `TestCheckRunning_LivePID` — write `os.Getpid()`, assert `(pid, true, nil)`.
- `TestWritePIDFile_Atomic` — pre-create target, assert full overwrite via
  tmp+rename semantics.
- `TestResolveLogOutput` — table-driven over `{userFlag, daemon}`.

### Spawn integration test (build-tag unix)

Uses the "test re-execs itself" pattern. `TestMain` branches on env vars:

- `HUBFUSE_TEST_CHILD=1` → write PID file, block on SIGTERM, exit 0.
- `HUBFUSE_TEST_DIE=1` → write `"boom"` to stderr, `os.Exit(2)`.
- `HUBFUSE_TEST_SLOW=1` → sleep without writing PID file.

Tests:

- `TestSpawn_Success` — parent returns nil, PID file exists and is live,
  SIGTERM cleans up.
- `TestSpawn_ChildDiesEarly` — error contains `"exited"` and `"boom"`.
- `TestSpawn_Timeout` (ReadyTimeout 500 ms) — error contains
  `"did not become ready"`, child killed.
- `TestSpawn_RemovesDaemonFlag` — verifies all four flag forms are
  stripped from the re-exec argv.

### `cmd/*` coverage

No Go tests. Covered by a manual smoke checklist included in the PR body:

```
# agent
hubfuse-agent start --daemon            # returns immediately
hubfuse-agent status                    # running (pid N)
hubfuse-agent devices                   # works in same terminal
hubfuse-agent stop                      # stops, pid file gone
hubfuse-agent start --daemon            # starts again
hubfuse-agent start --daemon            # -> already running (pid N), exit 1

# hub (same checklist)
hubfuse-hub start --daemon
hubfuse-hub status
hubfuse-hub stop
hubfuse-hub start --daemon
hubfuse-hub start --daemon              # -> already running (pid N)
```

## Files touched

- `internal/common/daemonize/daemonize.go` (new, `//go:build unix`)
- `internal/common/daemonize/daemonize_windows.go` (new, stub)
- `internal/common/daemonize/daemonize_test.go` (new)
- `internal/common/daemonize/spawn_test.go` (new, `//go:build unix`)
- `cmd/hubfuse-agent/main.go` — new flags, new `RunE` shape, PID-file
  write moved into `daemon.OnReady` callback.
- `cmd/hubfuse-hub/main.go` — new flag, same `RunE` shape, PID-file write
  supplied via `hub.OnReady`.
- `internal/agent/daemon.go` — add `OnReady func()` field on `Daemon`;
  call it once after step 4 (Register) in `Run`.
- `internal/hub/hub.go` — add `OnReady func()` field on `Hub`; replace
  the inline `os.WriteFile(pidFile, ...)` at lines 111–114 with a call to
  the callback.

No proto changes.

## Rollout

Single PR. `go vet`, `golangci-lint`, `go test ./...` all green. Manual
smoke checklist above executed on the author's machine. Issue #2 closed
by the merge commit.
