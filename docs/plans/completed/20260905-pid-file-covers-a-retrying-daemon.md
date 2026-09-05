# The PID file covers a retrying daemon (#102)

## Overview

Since #101 the daemon retries a failed first hub session instead of exiting. It writes its PID file
from `onReady`, fired inside `sessionOnce` only after the hub ACCEPTS a Register — so for the whole
retry window there is no PID file, and three things are wrong at once:

- `hubfuse stop` and `hubfuse status` report "not running" for a process that is running
  (`cmd/hubfuse/main.go:466`, `:530`);
- `hubfuse start` passes the `CheckRunning` guard (`main.go:404`) and the second daemon then dies on
  the SSH bind;
- `hubfuse start -d` and `hubfuse restart` do not get the retry at all: `Spawn` kills the child when
  no PID file appears within `ReadyTimeout` (`internal/common/daemonize/spawn_unix.go:130`).

The last one matters most. `-d` is how a person starts the daemon by hand, and against a down hub it
still ends with a dead daemon — the exact failure #101 set out to remove, surviving on the path most
likely to be used interactively.

## Context (from discovery)

- `internal/agent/daemon.go:910` — `readyOnce.Do` fires `onReady` and sets `everRegistered`, inside
  `sessionOnce`, after an accepted Register.
- `internal/agent/daemon.go:589` and `:1287` — the only readers of `everRegistered`: the clean-stop
  branch (is a `Shutdown` owed?) and the first-session log wording. Both want "the hub accepted a
  Register" and must keep that meaning.
- `Run`'s order is `connect` → `seedNicknamesFromHub` → `startSSH` → `reloadSSHAllowedKeys` →
  `guardConfiguredTargets` → `registerAndSubscribe`, so anything placed in `registerAndSubscribe`
  already has the SSH port bound and the mount targets guarded.
- `cmd/hubfuse/main.go:427` is the only agent-side `OnReady`, and it only writes the PID file.
  `runAgent` removes the file on every exit path (`defer os.Remove(pidPath)`).
- **Three assertions DO pin the PID file's timing, and two of them fail under this change** — all in
  `internal/agent/startupretry_test.go`, written by #101 a few hours ago:
  - `:148` (`TestRegisterAndSubscribe_StopIsCleanWhileStillRetrying`) and `:186`
    (`_SSHDeathEndsTheWait`) both `assert.Zero(t, ready.Load())` after a **non-refusal** error, so
    `markReady` fires and they go red;
  - `:79-80` (`_RetriesUntilTheHubAnswers`) still passes numerically, but its wording — "only for the
    Register the hub accepted" — becomes false.
  The first draft of this plan claimed no such pin existed. That is the **third** time in this
  codebase a plan has asserted a contract was unpinned when it was, so Task 1 names each assertion
  and its replacement rather than leaving an agent to discover them at `go test`.
- `tests/integration/stop_restart_test.go:31` drives `daemonize` against a `/bin/sh -c "sleep 60"`
  stand-in, never the agent binary; `tests/scenarios/prune_test.go:76` and
  `tests/scenarios/sshbind_test.go` use `StartDaemonExpectFailure`, which runs `hubfuse start` in the
  FOREGROUND and never touches `Spawn`. Both verified. So `start -d` is uncovered end to end.
- `spawnAgentDaemon` (`cmd/hubfuse/main.go:349`) does NOT set `ReadyTimeout`, so `Spawn` uses its 5s
  default — while `seedNicknamesFromHub` (`listDevicesTimeout` 10s) and `sessionOnce`
  (`registerTimeout` 20s) both sit ahead of the point this plan makes ready.

## Development Approach

- **testing approach**: Regular
- every task ends with tests, run before the next task starts
- `make test` green plus `make test-race` for `./internal/agent/...` before the PR
- config, CLI surface and plist unchanged

## Solution Overview

The PID file stops meaning "registered with the hub" and starts meaning **"this daemon is committed
to running"**. The two differ in exactly one place, and that place is the point of the change.

The naive fix — write it right after `startSSH` — is rejected, and the reason is a new lie it would
tell. A pruned identity exits on its first Register (`ErrHubRejected`, the #69 contract preserved by
#101). With the PID file written at bind time, `Spawn` would see the file, print
`started (pid N)`, return zero, and only then would the child hit the refusal and exit. The operator
would be told a dead daemon had started — the same shape of untruth #69 was about, moved one step
later.

Instead `readyOnce` fires at the moment the daemon **decides to keep running**:

- in `sessionOnce`, on an accepted Register — unchanged;
- in `registerAndSubscribe`, immediately before entering the retry loop — i.e. after a first attempt
  that failed with something other than a refusal.

`readyOnce` makes those exactly-once between them. A refused daemon reaches neither and still leaves
no PID file behind, so `Spawn` still sees the child exit and still reports the refusal with its log
tail.

`everRegistered` moves OUT of the `readyOnce` closure and is set on its own, only on an accepted
Register. It is not a synonym for readiness any more and its two readers must not drift: the
clean-stop branch decides whether a `Deregister` is owed, and a daemon that never registered owes
none.

### The residual window, stated rather than hidden

The argument against the naive fix applies in miniature to this one: after `markReady` fires,
`registerAndSubscribe` can still return an error through `case sshErr := <-d.sshDied`
(`daemon.go:843`), so a process `Spawn` has already reported as started can still exit non-zero.

It is accepted, and the reason is frequency, not principle. A bind failure — the common SSH failure,
and the whole of #90 — still aborts `Run` before `markReady` is ever reached (`daemon.go:564`). What
remains is an accept loop dying inside the retry window, which is rare and self-announcing. This is
exactly the case `startupretry_test.go:186` currently asserts the opposite of, which is why that
assertion is re-specified rather than deleted.

## Consequences, stated

- `hubfuse start -d` against a down hub now reports `started (pid N)` and the daemon retries in the
  background. That is the intended new behaviour and the reason for the change.
- **How long that takes depends on how the hub is unreachable, and the 5s default does not cover it.**
  A hub process that is down RSTs instantly and a NECP denial returns EHOSTUNREACH instantly, so
  readiness lands sub-second. A hub HOST that is off or black-holing packets does not: the first
  attempt is bounded by `listDevicesTimeout` (10s, `seedNicknamesFromHub`, which runs before the SSH
  bind) plus `registerTimeout` (20s), so readiness can be ~30s away. With the 5s default `Spawn`
  would kill the child in exactly the case an operator most wants to survive — a sleeping hub
  machine. `spawnAgentDaemon` therefore sets `ReadyTimeout` explicitly, above that worst case. The
  cost is that `start -d` against a black-holed hub prints nothing for up to ~30s before reporting
  success.
- If the FIRST attempt fails transiently and a LATER one is refused, the daemon is already inside
  `reconnectSession` and retries forever (the #101 asymmetry, unchanged) — but now holds a PID file,
  so `start -d` reports `started` for a device the hub is refusing. The refusal is logged at Error on
  every attempt; the PID file makes the process visible to `stop`, which is an improvement on it
  being invisible.
- `hubfuse status` reports a retrying daemon as running. Correct: it is.
- `hubfuse start` twice is now caught by the `CheckRunning` guard instead of failing later on the SSH
  bind.

## Implementation Steps

### Task 1: The PID file is written when the daemon commits to running

**Files:**
- Modify: `internal/agent/daemon.go`
- Modify: `internal/agent/startupretry_test.go`
- Modify: `cmd/hubfuse/main.go`

- [x] add `markReady()` wrapping `readyOnce.Do(onReady)`; move `everRegistered.Store(true)` out of
      the closure so it is set on its own, only on an accepted Register
- [x] call `markReady()` in `registerAndSubscribe` immediately before entering the retry loop —
      below the `ErrHubRejected` branch, so a refused daemon still reaches neither call
- [x] set `ReadyTimeout` explicitly in `spawnAgentDaemon` (`cmd/hubfuse/main.go:349`) above
      `listDevicesTimeout + registerTimeout`, with the arithmetic in the comment
- [x] re-specify `startupretry_test.go:148` and `:186`: `onReady` now FIRES in both, because a daemon
      stopped or killed mid-retry legitimately had a PID file — `runAgent`'s
      `defer os.Remove(pidPath)` (`cmd/hubfuse/main.go:436`) is what makes that safe
- [x] re-specify `startupretry_test.go:79-80` and rewrite the doc comments at `:51-58`, `:121-129`
      and `:154-163` that state the old contract
- [x] extend `_ExitsWhenTheHubRefusesTheFirstRegistration` instead of adding a fourth near-identical
      test: its existing `assert.Zero(t, ready.Load())` already carries the #69 property, so widen
      its comment to say `markReady` sits below the refusal branch
- [x] write a test for the new event: a transient first failure fires `onReady` exactly once, and
      `everRegistered` is FALSE at that moment — captured INSIDE the `onReady` callback, not checked
      afterwards, so it cannot go racy or vacuous. On revert `onReady` only ever runs with
      `everRegistered` true, so the assertion inverts.
- [x] rewrite the stale comments this change falsifies: `daemon.go:34-37` (`DaemonOptions.OnReady`,
      the public contract), `:733-736` (#90's "no PID file either"), `:795-806`
      (`registerAndSubscribe`'s "no PID file"), `:905-909` (the `readyOnce.Do` comment),
      `internal/common/daemonize/spawn_unix.go:135-142` and `TestSpawn_TimeoutCarriesTheLogTail`'s
      doc comment — an unreachable hub now reaches `Spawn`'s SUCCESS branch for the agent
- [x] run `go test ./internal/agent/... ./internal/common/...` and
      `go test ./tests/scenarios/ -run 'TestPrunedIdentityFailsToStart|TestSSHBind'`

### Task 2: Cover `hubfuse start -d` end to end

The harness has never run a detached start, and a `Setsid`'d child (`spawn_unix.go:97`) escapes every
cleanup it has: `Agent.Stop` kills the process GROUP, and it only knows about daemons that
`launchDaemon` started. A leaked daemon would hold the agent's SSH port and hammer the hub for the
rest of the test binary, and across `-count=N`.

**Files:**
- Create: `tests/scenarios/detached_start_test.go`

- [x] `StartHub` → `Join` → `hub.Stop(t)`, then run `hubfuse start -d` and assert it exits ZERO and
      prints a pid. Before this change it fails after `ReadyTimeout` with the child killed.
- [x] register a `t.Cleanup` FIRST that reads `$HOME/.hubfuse/agent.pid`, sends SIGTERM then SIGKILL,
      and tolerates the file being absent — the detached daemon cannot be reached by the harness's
      process-group kill
- [x] read the daemon's log from `$HOME/.hubfuse/agent.log`, not `a.logBuf`: a detached child's
      output goes to the file, so `WaitForDaemonLogCount` and `DumpOnFailure` are blind to it
- [x] assert `hubfuse status` OUTPUT contains "is running (pid" — `ReportStatus` returns nil
      unconditionally, so asserting a zero exit would be vacuous
- [x] assert `hubfuse stop` then removes the PID file. The file's ABSENCE is the primary assertion,
      because it is written by the daemon's own `defer os.Remove`; `SignalStop`'s own exit check
      polls `syscall.Kill(pid, 0)`, which CLAUDE.md rules out for this harness and which misreports
      an orphan under a PID 1 that does not reap.
- [x] do NOT re-assert hub-down → retry → `online`: `tests/scenarios/startup_retry_test.go` already
      owns that end to end, and repeating it would add ~45s for a property this change is not about
- [x] run `go test ./tests/scenarios/ -run TestDetachedStart`

### Task 3: Verify acceptance criteria

- [x] `make test` green
- [x] `make test-race` green for `./internal/agent/...`
- [x] `make vet` green
- [x] confirm `tests/scenarios/prune_test.go` still passes unchanged — a refused identity must still
      exit and leave no PID file

### Task 4: [Final] Documentation

- [x] CLAUDE.md: the `daemon.go` bullet gains what the PID file means now and why it is not
      `everRegistered`
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification on the test bed:**
- hub PROCESS stopped (host up, connections refused): `hubfuse status` on the Mac must report the
  retrying daemon as running, and `hubfuse stop` must stop it. Needs the hub down for ~30s.
- hub HOST unreachable (a bogus LAN address that black-holes, so the connect waits out its budget):
  `hubfuse start -d` must still report success rather than being killed. This is the case the
  explicit `ReadyTimeout` exists for, and 127.0.0.1 with a stopped hub cannot exercise it — it RSTs
  instantly.

**Filed rather than fixed here:**
- `seedNicknamesFromHub`'s 10s budget sits ahead of `startSSH` and the first Register, which is what
  makes the `ReadyTimeout` margin large. Reordering it touches #77's rationale and is scope creep.

**Not in scope:**
- #103 (the config watcher is inactive during the retry). Separate: `onConfigChange`'s `UpdateShares`
  needs a hub client, so a reload firing during the retry would call it against a connection that has
  never completed an RPC. That is its own bug and must not be folded in here.
