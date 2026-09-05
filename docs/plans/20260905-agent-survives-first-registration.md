# Agent survives a failed first registration (#74, #98)

> Revised after plan review. The first draft claimed "nothing pins the startup exit" and built its
> central decision on that. It is false — `tests/scenarios/prune_test.go:50`
> `TestPrunedIdentityFailsToStart` pins it directly, via `StartDaemonExpectFailure`, which fails the
> test if the process is still alive at the timeout. That single fact reverses the decision below,
> and three further findings (the exit code on the stop path, the #90 SSH-liveness invariant, and the
> startup diagnostic turning into noise) are folded in.

## Overview

The macOS agent dies if its very first hub registration fails. `Run` calls `registerAndSubscribe`,
which returns the first `sessionOnce` error, and `Run` returns it — the process exits. Meanwhile
`reconnectSession`, which runs for every *later* session, retries forever with backoff. The daemon
tolerates every failure except the first one.

That asymmetry is what makes the macOS local-network denial fatal. Measured on the test bed
(macOS 26.4): under a LaunchAgent, the first `connect()` from an identity macOS has not yet
registered is refused with `EHOSTUNREACH` — the kernel log names `reason: NECP` — and attempts
seconds later succeed. Two independent ad-hoc probes gave `fail, ok, ok` and `fail, fail, ok`.
Apple documents the shape (TN3179: the system "may deny the operation immediately before the user
responds") and has an open bug where a short-lived process dies before the grant lands (FB16131937).

The user's requirement, stated literally: when the Mac comes back, the agent must come back on its
own. Retry delivers that for every cause at once — a Mac waking, a network not up yet, a hub still
starting, and the NECP first-contact denial.

Two shipped artifacts of #74 are retracted here. Both are wrong:

- the error message tells the operator to approve hubfuse under System Settings → Privacy &
  Security → Local Network. A bare ad-hoc Mach-O cannot appear there: `nehelper` logs
  `Could not find bundle ID or display name for app: (bundleID: hubfuse-<hash>, name: (null),
  teamID: (null))`, so no entry can be constructed. The repo owner went looking and found nothing.
- the message and CLAUDE.md claim the decision "follows the PATH, not the file". Retracted as
  unproven, not as disproven — see *The path claim* below.

## The one decision the review reversed

**`ErrHubRejected` on the FIRST attempt still exits. Everything else retries.**

The first draft routed every startup failure into `reconnectSession`, on the stated ground that
nothing pinned the exit. `tests/scenarios/prune_test.go:50` pins it, and it is not an incidental
assertion: it is the whole of #69's first half — a device whose hub row was pruned still holds valid
TLS material, so it connects, registers at the transport level, and the hub answers `Success=false`.
That refusal used to be ignored and the daemon ran on as a ghost.

Keeping the exit is also the better behaviour, independent of the test:

- "the hub cannot be reached" is what retry is for. "the hub answered and said no" needs a human to
  run `hubfuse join`; no amount of retrying changes it.
- exiting costs nothing at startup — there is no session, no PID file, no mount, no peer that has
  seen this device.
- under launchd, `ThrottleInterval` (Task 4) turns the exit into one restart every 30s, each logging
  the hub's own instruction. That is a bounded, visible loop, not #98's 5-launches-a-minute.

The asymmetry with `reconnectSession` — which retries the same error mid-life — is deliberate and
worth stating: mid-life there IS something to lose. The daemon is registered, peers are mounted from
it, and tearing all that down for a refusal that may be a transient hub-side store error is worse
than logging it at Error and trying again.

Scope of the exit, precisely: only the FIRST attempt. If attempt 1 is transient and attempt 4
returns `ErrHubRejected`, the daemon is already inside `reconnectSession` and retries — the existing
mid-life behaviour, unchanged.

## Context (from discovery)

- `internal/agent/daemon.go:525` `Run` — returns on `registerAndSubscribe` error.
- `internal/agent/daemon.go:747` `registerAndSubscribe` — returns the first `sessionOnce` error.
- `internal/agent/daemon.go:1190` `reconnectSession` — infinite backoff (`backoffInitial` 1s →
  `backoffMax` 60s), `ErrHubRejected` at Error, the denial streak, the one-per-episode connection
  swap; returns nil only on `ctx.Done()`.
- `internal/agent/daemon.go:828` — `readyOnce` fires after a hub-ACCEPTED Register and writes the
  PID file. Verified: `onReady` is set only from `cmd/hubfuse/main.go:427`, so no PID file is ever
  written for a daemon the hub refused. That is the second half of #69 and it stays true.
- `internal/agent/connector.go:95` `Connect` — retries, but `grpc.NewClient` is lazy, so it cannot
  observe an unreachable hub. The first real failure is always the Register RPC.
- `internal/agent/daemon.go:1380` — `d.sshDied` is received ONLY in `runServices`, which is
  unreachable while `registerAndSubscribe` blocks. See Task 1.
- `tests/scenarios/helpers/hub.go:231` `Restart` — reuses the port and data dir; precedent for the
  stop/start sequence at `tests/scenarios/reconnect_test.go:36`.
- `tests/scenarios/helpers/agent.go:352` `launchDaemon` is unexported; scenarios call
  `StartDaemon`/`RestartDaemon`. Its wait is `WaitForPort` + the `ssh server listening` line, emitted
  by `SSHServer.Listen` before `registerAndSubscribe` — so a hubless start does reach it.

Tests and comments this change deliberately replaces (spec change, argued, not green-chased):

- `internal/agent/localnetwork_test.go:173` and `:196` — both require `registerAndSubscribe` to
  return an error on a transient failure. With retry they would block forever.
- `internal/agent/localnetwork_test.go:164` — asserts the message contains "path" and "rebuilding
  hubfuse in place does not produce a fresh prompt".
- `cmd/hubfuse/launchagent_test.go:110` — pins "the decision follows the PATH, not the file".

Unaffected, verified: `tests/scenarios/prune_test.go` (the exit is preserved),
`tests/scenarios/sshbind_test.go:44` (`startSSH` runs before registration, so that daemon still
exits), `tests/integration/liveness_test.go:82,88` (direct client calls), `internal/agent/swap_test.go`
and `daemon_test.go:1650` (drive `reconnectSession` directly).

## Development Approach

- **testing approach**: Regular
- every task ends with tests, run before the next task starts
- `make test` green plus `make test-race` for `./internal/agent/...` before the PR
- the config, the CLI surface and the plist label are unchanged

## Testing Strategy

- **unit** (`internal/agent`): N transient failures then success → `registerAndSubscribe` returns a
  live stream, `registerFn` called exactly N+1 times, `onReady` fired once and only after the
  accepted Register. `ErrHubRejected` on attempt 1 → returns the error, `registerFn` called exactly
  once, `onReady` never fired. Context cancelled mid-retry → returns cleanly, no stream, `onReady`
  never fired. SSH death mid-retry → returns the SSH error.
- **unit** (`cmd/hubfuse`): the generated plist pins `KeepAlive`/`ThrottleInterval` and no longer
  pins the retracted claim.
- **scenario**: the agent starts with the hub down and comes online when the hub returns.
- **test bed**: the NECP denial can no longer be reproduced there (TN3179: the privilege cannot be
  reset — FB14944392), but "hub down at agent start → hub up → online" can, by stopping and starting
  the hub on the Linux box while the LaunchAgent daemon runs. Required before merge.

## Progress Tracking

- mark completed items `[x]` immediately; ➕ for new tasks, ⚠️ for blockers

## Solution Overview

`registerAndSubscribe` returns the stream instead of starting `supervise` itself, so `Run` can tell
three outcomes apart:

1. a live stream → `go supervise`, then `runServices` as today;
2. an error → `Run` returns it (the `ErrHubRejected` and SSH-death paths);
3. no stream and no error → the context was cancelled while still trying.

Outcome 3 must exit **zero**. `reconnectSession` returns nil only on `ctx.Done()`, which is
SIGTERM/SIGINT — the ordinary stop. Returning `ctx.Err()` there would make `Run` return an error,
`runAgent` wrap it, `main` exit 1, and Task 4's `KeepAlive{SuccessfulExit:false}` relaunch the
daemon: #98 reproduced by the fix for #98. Two sub-cases, and they are not the same:

- **no Register was ever accepted** — return nil without `Shutdown`. There is nothing to deregister,
  and `Deregister` against an unreachable hub fails and aggregates into exactly the non-zero exit
  being avoided.
- **a Register was accepted on some attempt** (Register ok, Subscribe failed, back into the loop) —
  the PID file exists, the heartbeat runs, `processInitialDevices` has mounted, and the hub has this
  device online. That needs the normal `Shutdown()`.

A new `everRegistered` flag, set inside the existing `readyOnce`, is what separates them.

**The SSH-liveness invariant (#90) must survive the retry window.** `d.sshDied` is read only in
`runServices`. An accept loop that dies while the daemon is retrying would sit unread in the
buffered channel; registration would eventually succeed, the hub would be told `d.sshPort`, peers
would mount, and only then would `runServices` drain the channel and deregister. That is #90's harm
with a smaller window. So the retry runs in a goroutine and the wait selects on the stream and on
`sshDied`, returning the SSH error and aborting `Run` — the contract CLAUDE.md states as "a
precondition of the daemon existing at all, in both directions".

**The startup denial diagnostic is deleted, not kept.** It exists because "the daemon never reaches
the reconnect loop at all" — a reason this change removes. Keeping it would be actively harmful:
the measured NECP shape is `fail, ok, ok`, so it would log at Error on the first EHOSTUNREACH of
every fresh-identity launch and be contradicted by `registered with hub` a second later; and because
`localNetworkDeniedOnce` is once-per-process, that false positive would permanently suppress the
*real* streak message in a daemon that now lives for hours. `reconnectSession`'s streak of 3 already
covers the sustained case, which is the only case worth naming.

## Known limitations, recorded rather than discovered later

- **`hubfuse start -d` and `hubfuse restart` do not gain the retry.** `Spawn` kills the child when
  no PID file appears within `ReadyTimeout`, and the PID file is written only after an accepted
  Register. Against a down hub they still end with a dead daemon, now after ~5s instead of ~1s.
  Task 2 (done) makes that failure report the daemon's own reason instead of only a timeout.
- **`hubfuse stop` and `hubfuse status` report "not running" while a foreground daemon retries**,
  because there is no PID file yet. Under launchd — the deployment path — `launchctl bootout` works
  and Ctrl-C works in a terminal. Moving the PID file earlier would touch #69's contract and is
  deliberately out of scope; it is filed as follow-up work in Post-Completion.
- **Hot config reload is inactive during the retry window**, because the watcher starts in
  `runServices`. An operator who corrects a wrong `hub.address` must restart the daemon. Previously
  the daemon exited, so the next start read the corrected file. Recorded as a real, accepted
  regression.

## The path claim

CLAUDE.md records a control for the retracted claim: the same bytes, signed the same way, refused
instantly at a path macOS had already refused, and given the full grace window at a fresh path. The
confound is named rather than waved away: those two runs also differed in launch context — the
already-refused path was the installed `~/go/bin/hubfuse` started one way, the fresh path a copy
started another — and TN3179's exemption table makes launch context sufficient to explain both
outcomes on its own. The claim is therefore **unproven**, not disproven, and it is removed from
operator-facing text because acting on it (reinstall to a new path) is useless advice either way.

## Technical Details

```go
// Run
stream, err := d.registerAndSubscribe(ctx)
if err != nil {
    return err
}
if stream == nil {
    if d.everRegistered.Load() {
        return d.Shutdown()
    }
    d.logger.Info("daemon stopping before it ever registered with the hub")
    return nil
}
go d.supervise(ctx, stream)
return d.runServices(ctx, cancel)
```

```go
// registerAndSubscribe
stream, err := d.sessionOnce(ctx)
if err == nil {
    return stream, nil
}
if errors.Is(err, ErrHubRejected) {
    return nil, err // #69: the hub knows us and says no
}
d.logger.Warn("first hub session failed, retrying", "error", err)

result := make(chan pb.HubFuse_SubscribeClient, 1)
go func() { result <- d.reconnectSession(ctx) }()
select {
case s := <-result:
    return s, nil // nil == ctx cancelled == clean stop
case sshErr := <-d.sshDied:
    return nil, fmt.Errorf("ssh server died before the daemon reached the hub: %w", sshErr)
}
```

`reconnectSession` logs `"hub session re-established"` on success, which is false for a session that
never existed. It takes a `first bool` (or the call site logs) so the first-ever success reads
correctly.

New plist keys:

```
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>30</integer>
```

`SuccessfulExit: false` restarts only on a non-zero exit, so `hubfuse stop` (SIGTERM → exit 0)
actually stops the daemon instead of having launchd bring it straight back. `ThrottleInterval` 30
bounds a crash loop to two launches a minute.

## Implementation Steps

### Task 1: The first session retries, except when the hub refuses

**Files:**
- Modify: `internal/agent/daemon.go`
- Modify: `internal/agent/localnetwork_test.go`
- Create: `internal/agent/startupretry_test.go`

- [ ] add `everRegistered atomic.Bool`, set inside the existing `readyOnce` block in `sessionOnce`
- [ ] change `registerAndSubscribe` to return `(pb.HubFuse_SubscribeClient, error)`, exit on a
      first-attempt `ErrHubRejected`, otherwise retry via `reconnectSession` in a goroutine while
      selecting on `d.sshDied`
- [ ] move `go d.supervise(...)` into `Run` and add the three-outcome branch, including the
      `everRegistered` split on the cancelled path
- [ ] make `reconnectSession`'s success log read correctly for a first-ever session
- [ ] delete the startup denial call site and `localNetworkEvidenceStartup`
- [ ] write a test: 2 transient failures then success → returns a stream, `registerFn` called
      exactly 3 times, `onReady` fired exactly once
- [ ] write a test: `ErrHubRejected` on attempt 1 → returns that error, `registerFn` called exactly
      once, `onReady` never fired
- [ ] write a test: context cancelled mid-retry → nil stream, nil error, `onReady` never fired
- [ ] write a test: `sshDied` fires mid-retry → returns the SSH error
- [ ] rewrite `TestRegisterAndSubscribe_NamesLocalNetworkDenialAtStartup` and
      `_StaysQuietOnAnOrdinaryStartupFailure` against the new spec, or delete them if the streak
      test in `reconnectSession` now covers what they asserted
- [ ] run `go test ./internal/agent/...` and `go test ./tests/scenarios/ -run TestPrunedIdentity`
      — both must pass before Task 3

### Task 2: Spawn's readiness timeout reports the cause — DONE

**Files:**
- Modify: `internal/common/daemonize/spawn_unix.go`
- Modify: `internal/common/daemonize/spawn_unix_test.go`

- [x] include the log tail in the timeout error
- [x] test that the timeout error carries the daemon's own line; verified it fails without the fix

### Task 3: Retract the two wrong claims from the denial message

**Files:**
- Modify: `internal/agent/localnetwork.go`
- Modify: `internal/agent/localnetwork_test.go`

- [ ] rewrite `localNetworkDenialMessage`: name NECP, say the daemon is retrying, say to allow a
      Local Network prompt if one appears, attribute the `com.apple.network.local-network` CIDR keys
      to TN3179 rather than asserting them
- [ ] remove the "per binary PATH" and "rebuilding in place" claims and fix the `isLocalNetworkDenial`
      doc comment to state only what the logs show
- [ ] rewrite the message test to pin the new content AND assert the retracted phrases are absent
- [ ] run `go test ./internal/agent/...` — must pass before Task 4

### Task 4: LaunchAgent stops restart-looping (#98)

**Files:**
- Modify: `cmd/hubfuse/launchagent.go`
- Modify: `cmd/hubfuse/launchagent_test.go`

- [ ] replace `KeepAlive: true` with `SuccessfulExit: false`, add `ThrottleInterval` 30
- [ ] rewrite the instructions `install-agent` prints to match the new message
- [ ] remove the retracted "follows the PATH" claim from the file and its test
- [ ] write a test pinning `KeepAlive`/`ThrottleInterval` content, including that a zero exit must
      NOT trigger a restart
- [ ] run `go test ./cmd/...` — must pass before Task 5

### Task 5: Scenario — the agent starts with the hub down and comes up when it returns

**Files:**
- Create: `tests/scenarios/startup_retry_test.go`

- [ ] `StartHub` → `Join` (the cert exchange needs a live hub) → `hub.Stop(t)`
- [ ] `StartDaemon` with the hub down; the launch wait is satisfied by the `ssh server listening`
      line, which precedes registration
- [ ] wait for 3 `"hub session reconnect failed, retrying"` lines via `WaitForDaemonLogCount` — this
      is both the aliveness proof and the positive control that retry actually ran, and it avoids
      `syscall.Kill(pid, 0)`, which CLAUDE.md forbids for this harness
- [ ] `hub.Restart(t)` after ~3 retries (~8s in, while backoff is still ≤8s — the package timeout is
      300s and backoff caps at 60s, so waiting longer risks the suite)
- [ ] assert the device reaches `online` within a bounded wait
- [ ] run the scenario suite — must pass before Task 6

### Task 6: Verify acceptance criteria

- [ ] `make test` green
- [ ] `make test-race` green for `./internal/agent/...`
- [ ] `make vet` and the linter green
- [ ] grep for "follows the PATH", "per binary", "System Settings" outside `docs/plans/completed/`
      and confirm nothing operator-facing still asserts them

### Task 7: [Final] Documentation

- [ ] CLAUDE.md `localnetwork.go` paragraph: NECP, the `nehelper` evidence, the older unreconciled
      evidence named as unreconciled, retry as the fix
- [ ] CLAUDE.md `client.go` bullet: "a pruned identity now fails to start" stays TRUE (the exit is
      preserved) — verify the wording still matches after Task 1
- [ ] CLAUDE.md `daemon.go` #90 bullet: extend it to say the SSH-death watch now also covers the
      pre-registration retry window
- [ ] README.md macOS section: drop the impossible System Settings instruction, add the TN3179 CIDR
      keys as Apple's documented option
- [ ] move this plan to `docs/plans/completed/` (in this branch, before the merge)

## Post-Completion

**Manual verification on the test bed:**
- stop the hub, start the Mac daemon under its LaunchAgent, confirm it stays alive and logs retries;
  start the hub; confirm `hubfuse devices` shows `mac online` without touching the Mac.
- confirm `hubfuse stop` stops the LaunchAgent-managed daemon (the `SuccessfulExit` fix).

**Follow-up issues to file:**
- the PID file is written only after an accepted Register, so `hubfuse stop`/`status` are blind to a
  retrying daemon. Decide whether the PID file should mean "a daemon process is running" instead.
- hot config reload is inactive during the retry window.

**Housekeeping on the test-bed Mac:**
- `~/Applications/HubFuseProbe.app` stays until the owner has finished inspecting the Local Network
  pane; the probe LaunchAgents and `/tmp/lnptest` are already removed. Any stale `soak.sh` /
  `probe-*` entries under Login Items → "Allow in the Background" are from this session's probes and
  can be removed.

**Not in scope, recorded deliberately:**
- shipping the agent as a `.app` bundle. It is the only measured way to make the binary listable in
  System Settings, but the owner declined the build change and the measurements show access does not
  require it (a bare ad-hoc binary under a LaunchAgent: 18/18 LAN connects over 4 minutes, no
  approval, no session).
